/*
© 2021 Red Hat, Inc. and others.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	subv1 "github.com/submariner-io/submariner/pkg/apis/submariner.io/v1"

	"github.com/submariner-io/submariner-operator/apis/submariner/v1alpha1"
)

const (
	ClientSourcePort = "9898"
)

var verboseOutput bool

var validateTunnelCmd = &cobra.Command{
	Use:   "tunnel <localkubeconfig> <remotekubeconfig>",
	Short: "Validate if firewall allows tunnels on the Gateway node.",
	Long:  "This command checks whether firewall configuration allows tunnel to be configured on the Gateway nodes.",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return fmt.Errorf("two kubeconfigs must be specified")
		}
		same, err := compareFiles(args[0], args[1])
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("kubeconfig file <localkubeconfig> and <remotekubeconfig> need to have" +
				" a unique content")
		}
		return nil
	},
	Run: validateTunnelConfig,
}

func init() {
	addValidateFWConfigFlags(validateTunnelCmd)
	validateTunnelCmd.Flags().BoolVar(&verboseOutput, "verbose", false,
		"produce verbose logs during validation")
	validateFirewallConfigCmd.AddCommand(validateTunnelCmd)
}

func validateTunnelConfig(cmd *cobra.Command, args []string) {
	localCfg, err := getRestConfig(args[0], "")
	exitOnError("The provided local kubeconfig is invalid", err)

	remoteCfg, err := getRestConfig(args[1], "")
	exitOnError("The provided remote kubeconfig is invalid", err)

	validateTunnelConfigAcrossClusters(localCfg, remoteCfg)
	status.End(status.ResultFromMessages())
}

func validateTunnelConfigAcrossClusters(localCfg, remoteCfg *rest.Config) {
	lClientSet, err := kubernetes.NewForConfig(localCfg)
	exitOnError("Error creating API server client:: %s", err)

	submariner := getSubmarinerResource(localCfg)
	if submariner == nil {
		exitWithErrorMsg(submMissingMessage)
	}

	status.Start(fmt.Sprintf("Validating if tunnels can be setup on Gateway node of cluster %q.",
		submariner.Spec.ClusterID))

	localEndpoint := getEndpointResource(localCfg, submariner.Spec.ClusterID)
	if localEndpoint == nil {
		status.QueueWarningMessage("Could not find the local cluster Endpoint")
		return
	}

	tunnelPort, err := getTunnelPort(submariner, localEndpoint)
	if err != nil {
		return
	}

	podCommand := fmt.Sprintf("timeout %d tcpdump -ln -nnX -s 100 -c 5 -i any udp and src port %s and dst port %d",
		validationTimeout, ClientSourcePort, tunnelPort)
	sPod, err := spawnSnifferPodOnNode(lClientSet, localEndpoint.Spec.Hostname,
		namespace, podCommand)
	if err != nil {
		status.QueueFailureMessage(fmt.Sprintf("Error while spawning the sniffer pod on the GatewayNode: %v", err))
		return
	}

	defer sPod.DeletePod()
	var gatewayPodIP string
	if !localEndpoint.Spec.NATEnabled {
		gatewayPodIP = localEndpoint.Spec.PrivateIP
	} else {
		gatewayPodIP = localEndpoint.Spec.PublicIP
	}

	rClientSet, err := kubernetes.NewForConfig(remoteCfg)
	if err != nil {
		message := fmt.Sprintf("Error creating API server client: %s", err)
		status.QueueFailureMessage(message)
		return
	}

	clientMessage := string(uuid.NewUUID())[0:8]
	podCommand = fmt.Sprintf("for x in $(seq 1000); do echo %s; done | for i in $(seq 5);"+
		" do timeout 2 nc -n -p %s -u %s %d; done", clientMessage, ClientSourcePort, gatewayPodIP, tunnelPort)
	// Spawn the pod on the nonGateway node. If we spawn the pod on Gateway node, the tunnel process can
	// sometimes drop the udp traffic from client pod until the tunnels are properly setup.
	cPod, err := spawnClientPodOnNonGatewayNode(rClientSet, namespace, podCommand)
	if err != nil {
		status.QueueFailureMessage(fmt.Sprintf("Error while spawning the client pod on non-Gateway node: %v", err))
		return
	}

	defer cPod.DeletePod()
	if err = cPod.AwaitPodCompletion(); err != nil {
		status.QueueFailureMessage(fmt.Sprintf("Error while waiting for client pod to be finish its execution: %v", err))
		return
	}

	if err = sPod.AwaitPodCompletion(); err != nil {
		status.QueueFailureMessage(fmt.Sprintf("Error while waiting for sniffer pod to be finish its execution: %v", err))
		return
	}

	if verboseOutput {
		status.QueueSuccessMessage(fmt.Sprintf("tcpdump output on Gateway node: %s", sPod.PodOutput))
	}

	validateSnifferPodOutput(sPod.PodOutput, clientMessage, localEndpoint.Spec.Hostname, tunnelPort)
	status.QueueSuccessMessage("Tunnels can be successfully established on the Gateway node.")
}

func getTunnelPort(submariner *v1alpha1.Submariner, endpoint *subv1.Endpoint) (int32, error) {
	var tunnelPort int32
	var err error
	switch submariner.Spec.CableDriver {
	case "libreswan", "wireguard":
		tunnelPort, err = endpoint.Spec.GetBackendPort(subv1.UDPPortConfig, int32(submariner.Spec.CeIPSecNATTPort))
		if err != nil {
			status.QueueWarningMessage(fmt.Sprintf("Error reading tunnelPort: %v", err))
		}
		return tunnelPort, nil
	default:
		message := fmt.Sprintf("Error parsing the tunnelPort for CableDriver %q",
			submariner.Spec.CableDriver)
		status.QueueWarningMessage(message)
		return tunnelPort, fmt.Errorf(message)
	}
}

func validateSnifferPodOutput(podOutput, clientMessage, hostname string, tunnelPort int32) {
	if !strings.Contains(podOutput, clientMessage) {
		message := fmt.Sprintf("The tcpdump output from the sniffer pod does not include the message"+
			" sent from client pod. Please check that your firewall configuration allows UDP/%d traffic"+
			" on the %q node.", tunnelPort, hostname)
		status.QueueFailureMessage(message)
	}

	if !strings.Contains(podOutput, ClientSourcePort) {
		message := fmt.Sprintf("The tcpdump output from the sniffer pod does not include packets with"+
			" sourcePort used by client pod. Please check that your firewall configuration allows UDP/%d traffic"+
			" on the %q node.", tunnelPort, hostname)
		status.QueueFailureMessage(message)
	}
}
