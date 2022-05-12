/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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

package azure

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/pkg/errors"
	"github.com/submariner-io/admiral/pkg/reporter"
	"github.com/submariner-io/admiral/pkg/util"
	"github.com/submariner-io/cloud-prepare/pkg/api"
	"github.com/submariner-io/cloud-prepare/pkg/azure"
	"github.com/submariner-io/cloud-prepare/pkg/k8s"
	"github.com/submariner-io/cloud-prepare/pkg/ocp"
	"github.com/submariner-io/subctl/internal/restconfig"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type Config struct {
	Gateways        int
	InfraID         string
	Region          string
	OcpMetadataFile string
	AuthFile        string
	GWInstanceType  string
}

func RunOn(restConfigProducer *restconfig.Producer, config *Config, status reporter.Interface,
	function func(api.Cloud, api.GatewayDeployer, reporter.Interface) error,
) error {
	if config.OcpMetadataFile != "" {
		var err error

		config.InfraID, config.Region, err = ReadFromFile(config.OcpMetadataFile)
		if err != nil {
			return status.Error(err, "Failed to read AWS information from OCP metadata file %q", config.OcpMetadataFile)
		}
	}

	status.Start("Retrieving Azure credentials from your Azure authorization file")

	err := os.Setenv("AZURE_AUTH_LOCATION", config.AuthFile)
	if err != nil {
		return status.Error(err, "Error locating authorization file")
	}

	subscriptionID, err := initializeFromAuthFile(config.AuthFile)
	if err != nil {
		return status.Error(err, "Failed to read authorization information from Azure authorization file")
	}

	status.End()

	status.Start("Initializing AWS connectivity")

	// This is the most recommended of several authentication options
	// https://github.com/Azure/go-autorest/tree/master/autorest/azure/auth#more-authentication-details
	authorizer, err := auth.NewAuthorizerFromEnvironment()
	if err != nil {
		return status.Error(err, "Error getting an authorizer for Azure")
	}

	k8sConfig, err := restConfigProducer.ForCluster()
	if err != nil {
		return status.Error(err, "Failed to initialize a Kubernetes config")
	}

	clientSet, err := kubernetes.NewForConfig(k8sConfig.Config)
	if err != nil {
		return status.Error(err, "Failed to create Kubernetes client")
	}

	k8sClientSet := k8s.NewInterface(clientSet)

	restMapper, err := util.BuildRestMapper(k8sConfig.Config)
	if err != nil {
		return status.Error(err, "Failed to create restmapper")
	}

	dynamicClient, err := dynamic.NewForConfig(k8sConfig.Config)
	if err != nil {
		return status.Error(err, "Failed to create dynamic client")
	}

	msDeployer := ocp.NewK8sMachinesetDeployer(restMapper, dynamicClient)

	cloudInfo := azure.CloudInfo{
		SubscriptionID: subscriptionID,
		InfraID:        config.InfraID,
		Region:         config.Region,
		BaseGroupName:  config.InfraID + "-rg",
		Authorizer:     authorizer,
		K8sClient:      k8sClientSet,
	}

	azureCloud := azure.NewCloud(&cloudInfo)

	status.End()

	gwDeployer, err := azure.NewOcpGatewayDeployer(azureCloud, msDeployer, config.GWInstanceType)
	if err != nil {
		return status.Error(err, "Failed to initialize a GatewayDeployer config")
	}

	return function(azureCloud, gwDeployer, status)
}

func ReadFromFile(metadataFile string) (string, string, error) {
	fileInfo, err := os.Stat(metadataFile)
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to stat file %q", metadataFile)
	}

	if fileInfo.IsDir() {
		metadataFile = filepath.Join(metadataFile, "metadata.json")
	}

	data, err := os.ReadFile(metadataFile)
	if err != nil {
		return "", "", errors.Wrapf(err, "error reading file %q", metadataFile)
	}

	var metadata struct {
		InfraID string `json:"infraID"`
		Azure   struct {
			Region string `json:"region"`
		} `json:"azure"`
	}

	err = json.Unmarshal(data, &metadata)
	if err != nil {
		return "", "", errors.Wrap(err, "error unmarshalling data")
	}

	return metadata.InfraID, metadata.Azure.Region, nil
}

func initializeFromAuthFile(authFile string) (string, error) {
	data, err := os.ReadFile(authFile)
	if err != nil {
		return "", errors.Wrapf(err, "error reading file %q", authFile)
	}

	var authInfo struct {
		ClientID       string
		ClientSecret   string
		SubscriptionID string
		TenantID       string
	}

	err = json.Unmarshal(data, &authInfo)
	if err != nil {
		return "", errors.Wrap(err, "error unmarshalling data")
	}

	if err := os.Setenv("AZURE_CLIENT_ID", authInfo.ClientID); err != nil {
		return "", errors.Wrapf(err, "Unable to set Azure Client ID env variable")
	}

	if err := os.Setenv("AZURE_CLIENT_SECRET", authInfo.ClientSecret); err != nil {
		return "", errors.Wrapf(err, "Unable to set Azure Client Secret env variable")
	}

	if err := os.Setenv("AZURE_TENANT_ID", authInfo.TenantID); err != nil {
		return "", errors.Wrapf(err, "Unable to set Azure Tenant ID env variable")
	}

	return authInfo.SubscriptionID, nil
}
