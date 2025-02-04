/*
 *  Copyright (c) 2024, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

/*
 * Package "synchronizer" contains artifacts relate to fetching APIs and
 * API related updates from the control plane event-hub.
 * This file contains functions to retrieve APIs and API updates.
 */

package synchronizer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"

	"github.com/wso2/product-apim-tooling/apim-apk-agent/config"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dpv1alpha1 "github.com/wso2/apk/common-go-libs/apis/dp/v1alpha1"
	dpv1alpha2 "github.com/wso2/apk/common-go-libs/apis/dp/v1alpha2"
	"github.com/wso2/apk/common-go-libs/utils"
	internalk8sClient "github.com/wso2/product-apim-tooling/apim-apk-agent/internal/k8sClient"
	logger "github.com/wso2/product-apim-tooling/apim-apk-agent/pkg/loggers"
	"github.com/wso2/product-apim-tooling/apim-apk-agent/pkg/logging"
	sync "github.com/wso2/product-apim-tooling/apim-apk-agent/pkg/synchronizer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	zipExt          string = ".zip"
	defaultCertPath string = "/home/wso2/security/controlplane.pem"
)

func init() {
	conf, _ := config.ReadConfigs()
	sync.InitializeWorkerPool(conf.ControlPlane.RequestWorkerPool.PoolSize, conf.ControlPlane.RequestWorkerPool.QueueSizePerPool,
		conf.ControlPlane.RequestWorkerPool.PauseTimeAfterFailure, conf.Agent.TrustStore.Location,
		conf.ControlPlane.SkipSSLVerification, conf.ControlPlane.HTTPClient.RequestTimeOut, conf.ControlPlane.RetryInterval,
		conf.ControlPlane.ServiceURL, conf.ControlPlane.Username, conf.ControlPlane.Password)
}

// FetchAPIsFromControlPlane method pulls API data for a given APIs according to a
// given API ID and a list of environments that API has been deployed to.
// updatedAPIID is the corresponding ID of the API in the form of an UUID
// updatedEnvs contains the list of environments the API deployed to.
func FetchAPIsFromControlPlane(updatedAPIID string, updatedEnvs []string) {
	// Read configurations and derive the eventHub details
	conf, errReadConfig := config.ReadConfigs()
	if errReadConfig != nil {
		// This has to be error. For debugging purpose info
		logger.LoggerSync.Errorf("Error reading configs: %v", errReadConfig)
	}
	// Populate data from config.
	configuredEnvs := conf.ControlPlane.EnvironmentLabels
	//finalEnvs contains the actual envrionments that the adapter should update
	var finalEnvs []string
	if len(configuredEnvs) > 0 {
		// If the configuration file contains environment list, then check if then check if the
		// affected environments are present in the provided configs. If so, add that environment
		// to the finalEnvs slice
		for _, updatedEnv := range updatedEnvs {
			for _, configuredEnv := range configuredEnvs {
				if updatedEnv == configuredEnv {
					finalEnvs = append(finalEnvs, updatedEnv)
				}
			}
		}
	} else {
		// If the labels are not configured, publish the APIS to the default environment
		finalEnvs = []string{config.DefaultGatewayName}
	}

	if len(finalEnvs) == 0 {
		// If the finalEnvs is empty -> it means, the configured envrionments  does not contains the affected/updated
		// environments. If that's the case, then APIs should not be fetched from the adapter.
		return
	}

	c := make(chan sync.SyncAPIResponse)
	logger.LoggerSync.Infof("API %s is added/updated to APIList for label %v", updatedAPIID, updatedEnvs)
	var queryParamMap map[string]string

	go sync.FetchAPIs(&updatedAPIID, finalEnvs, c, sync.RuntimeArtifactEndpoint, true, nil, queryParamMap)
	for {
		data := <-c
		logger.LoggerSync.Infof("Receiving data for the API: %q", updatedAPIID)
		if data.Resp != nil {
			// For successfull fetches, data.Resp would return a byte slice with API project(s)
			logger.LoggerSync.Infof("API Project %q", data.Resp)
			// err := PushAPIProjects(data.Resp, finalEnvs)
			// if err != nil {
			// 	logger.LoggerSync.Errorf("Error occurred while pushing API data for the API %q: %v ", updatedAPIID, err)
			// }
			break
		} else if data.ErrorCode >= 400 && data.ErrorCode < 500 {
			logger.LoggerSync.Errorf("Error occurred when retrieving API %q from control plane: %v", updatedAPIID, data.Err)
			//health.SetControlPlaneRestAPIStatus(false)
		} else {
			// Keep the iteration still until all the envrionment response properly.
			logger.LoggerSync.Errorf("Error occurred while fetching data from control plane for the API %q: %v. Hence retrying..", updatedAPIID, data.Err)
			sync.RetryFetchingAPIs(c, data, sync.RuntimeArtifactEndpoint, true, queryParamMap)
		}
	}

}

// FetchAPIsOnEvent  will fetch API from control plane during the API Notification Event
func FetchAPIsOnEvent(conf *config.Config, apiUUIDList []string, k8sClient client.Client) {
	// Populate data from config.
	envs := conf.ControlPlane.EnvironmentLabels

	// Create a channel for the byte slice (response from the APIs from control plane)
	c := make(chan sync.SyncAPIResponse)

	var queryParamMap map[string]string
	//Get API details.
	if apiUUIDList != nil {
		GetAPI(c, nil, envs, sync.APIArtifactEndpoint, true, apiUUIDList, queryParamMap)
	}
	for i := 0; i < 1; i++ {
		data := <-c
		logger.LoggerMsg.Info("Receiving data for an API")
		if data.Resp != nil {
			// Reading the root zip
			zipReader, err := zip.NewReader(bytes.NewReader(data.Resp), int64(len(data.Resp)))

			// apiFiles represents zipped API files fetched from API Manager
			apiFiles := make(map[string]*zip.File)
			// Read the .zip files within the root apis.zip and add apis to apiFiles array.
			for _, file := range zipReader.File {
				apiFiles[file.Name] = file
				logger.LoggerSync.Infof("API file found: " + file.Name)
				// Todo: Read the apis.zip and extract the api.zip,deployments.json
			}

			envConfig1 := dpv1alpha2.EnvConfig{
				HTTPRouteRefs: []string{"route1", "route2"},
			}

			// Set up the API object
			api := &dpv1alpha2.API{
				TypeMeta: metav1.TypeMeta{
					Kind:       "API",
					APIVersion: "dp.wso2.com/v1alpha2",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:              apiUUIDList[0],
					Namespace:         utils.GetOperatorPodNamespace(),
					CreationTimestamp: metav1.Now(),
				},
				Spec: dpv1alpha2.APISpec{
					APIName:          "sample-api",
					APIVersion:       "v1",
					APIType:          "REST",
					DefinitionPath:   "/docs",
					BasePath:         "/" + apiUUIDList[0] + "/v1",
					IsDefaultVersion: true,
					Organization:     "default",
					Sandbox:          []dpv1alpha2.EnvConfig{envConfig1},
					Production:       []dpv1alpha2.EnvConfig{envConfig1},
					APIProperties:    []dpv1alpha2.Property{},
				},
				Status: dpv1alpha2.APIStatus{},
			}

			configMap := &corev1.ConfigMap{}

			httpRoute := &gwapiv1b1.HTTPRoute{}

			secret := &corev1.Secret{}

			authPolicy := &dpv1alpha2.Authentication{}

			backendJWT := &dpv1alpha1.BackendJWT{}

			apiPolicies := &dpv1alpha2.APIPolicy{}

			interceptorServices := &dpv1alpha1.InterceptorService{}

			scope := &dpv1alpha1.Scope{}

			rateLimitPolicies := &dpv1alpha1.RateLimitPolicy{}

			backends := &dpv1alpha1.Backend{}

			// Apply the API to the Kubernetes cluster
			internalk8sClient.CreateAPICR(api, k8sClient)
			// Apply the ConfigMap to the Kubernetes cluster
			internalk8sClient.CreateConfigMapCR(configMap, k8sClient)
			// Apply the HttpRoute to the Kubernetes cluster
			internalk8sClient.CreateHTTPRouteCR(httpRoute, k8sClient)
			// Apply the Secret to the Kubernetes cluster
			internalk8sClient.CreateSecretCR(secret, k8sClient)
			// Apply the AuthPolicy to the Kubernetes cluster
			internalk8sClient.CreateAuthenticationCR(authPolicy, k8sClient)
			// Apply the BackendJWT to the Kubernetes cluster
			internalk8sClient.CreateBackendJWTCR(backendJWT, k8sClient)
			// Apply the APIPolicies to the Kubernetes cluster
			internalk8sClient.CreateAPIPolicyCR(apiPolicies, k8sClient)
			// Apply the InterceptorServices to the Kubernetes cluster
			internalk8sClient.CreateInterceptorServicesCR(interceptorServices, k8sClient)
			// Apply the Scope to the Kubernetes cluster
			internalk8sClient.CreateScopeCR(scope, k8sClient)
			// Apply the RateLimitPolicies to the Kubernetes cluster
			internalk8sClient.CreateRateLimitPolicyCR(rateLimitPolicies, k8sClient)
			// Apply the Backends to the Kubernetes cluster
			internalk8sClient.CreateBackendCR(backends, k8sClient)

			logger.LoggerMsg.Info("API applied successfully.\n")

			if err != nil {
				logger.LoggerMsg.Error("Error while reading zip", err)
			}
			//health.SetControlPlaneRestAPIStatus(err == nil)

		} else if data.ErrorCode == 204 {
			logger.LoggerMsg.Infof("No API Artifacts are available in the control plane for the envionments :%s",
				strings.Join(envs, ", "))
			//health.SetControlPlaneRestAPIStatus(true)
		} else if data.ErrorCode >= 400 && data.ErrorCode < 500 {
			logger.LoggerMsg.ErrorC(logging.ErrorDetails{
				Message:   fmt.Sprintf("Error occurred when retrieving APIs from control plane(unrecoverable error): %v", data.Err.Error()),
				Severity:  logging.CRITICAL,
				ErrorCode: 1106,
			})
			//isNoAPIArtifacts := data.ErrorCode == 404 && strings.Contains(data.Err.Error(), "No Api artifacts found")
			//health.SetControlPlaneRestAPIStatus(isNoAPIArtifacts)
		} else {
			// Keep the iteration still until all the envrionment response properly.
			i--
			logger.LoggerMsg.ErrorC(logging.ErrorDetails{
				Message:   fmt.Sprintf("Error occurred while fetching data from control plane: %v ..retrying..", data.Err),
				Severity:  logging.MINOR,
				ErrorCode: 1107,
			})
			//health.SetControlPlaneRestAPIStatus(false)
			sync.RetryFetchingAPIs(c, data, sync.RuntimeArtifactEndpoint, true, queryParamMap)
		}
	}
	logger.LoggerMsg.Info("Fetching API for an event is completed...")
}

// GetAPI function calls the FetchAPIs() with relevant environment labels defined in the config.
func GetAPI(c chan sync.SyncAPIResponse, id *string, envs []string, endpoint string, sendType bool, apiUUIDList []string,
	queryParamMap map[string]string) {
	if len(envs) > 0 {
		// If the envrionment labels are present, call the controle plane with labels.
		logger.LoggerAdapter.Debugf("Environment labels present: %v", envs)
		go sync.FetchAPIs(id, envs, c, endpoint, sendType, apiUUIDList, queryParamMap)
	} else {
		// If the environments are not give, fetch the APIs from default envrionment
		logger.LoggerAdapter.Debug("Environments label  NOT present. Hence adding \"default\"")
		envs = append(envs, "default")
		go sync.FetchAPIs(id, nil, c, endpoint, sendType, apiUUIDList, queryParamMap)
	}
}
