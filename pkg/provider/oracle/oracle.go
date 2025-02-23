/*
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
package oracle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/keymanagement"
	"github.com/oracle/oci-go-sdk/v65/secrets"
	"github.com/tidwall/gjson"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	esmeta "github.com/external-secrets/external-secrets/apis/meta/v1"
	"github.com/external-secrets/external-secrets/pkg/provider/aws/util"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

const (
	errOracleClient                          = "cannot setup new oracle client: %w"
	errORACLECredSecretName                  = "invalid oracle SecretStore resource: missing oracle APIKey"
	errUninitalizedOracleProvider            = "provider oracle is not initialized"
	errInvalidClusterStoreMissingSKNamespace = "invalid ClusterStore, missing namespace"
	errFetchSAKSecret                        = "could not fetch SecretAccessKey secret: %w"
	errMissingPK                             = "missing PrivateKey"
	errMissingUser                           = "missing User ID"
	errMissingTenancy                        = "missing Tenancy ID"
	errMissingRegion                         = "missing Region"
	errMissingFingerprint                    = "missing Fingerprint"
	errMissingVault                          = "missing Vault"
	errJSONSecretUnmarshal                   = "unable to unmarshal secret: %w"
	errMissingKey                            = "missing Key in secret: %s"
	errUnexpectedContent                     = "unexpected secret bundle content"
)

// https://github.com/external-secrets/external-secrets/issues/644
var _ esv1beta1.SecretsClient = &VaultManagementService{}
var _ esv1beta1.Provider = &VaultManagementService{}

type VaultManagementService struct {
	Client                VMInterface
	KmsVaultClient        KmsVCInterface
	vault                 string
	workloadIdentityMutex sync.Mutex
}

type VMInterface interface {
	GetSecretBundleByName(ctx context.Context, request secrets.GetSecretBundleByNameRequest) (secrets.GetSecretBundleByNameResponse, error)
}

type KmsVCInterface interface {
	GetVault(ctx context.Context, request keymanagement.GetVaultRequest) (response keymanagement.GetVaultResponse, err error)
}

// Not Implemented PushSecret.
func (vms *VaultManagementService) PushSecret(_ context.Context, _ []byte, _ *apiextensionsv1.JSON, _ esv1beta1.PushRemoteRef) error {
	return fmt.Errorf("not implemented")
}

func (vms *VaultManagementService) DeleteSecret(_ context.Context, _ esv1beta1.PushRemoteRef) error {
	return fmt.Errorf("not implemented")
}

// Empty GetAllSecrets.
func (vms *VaultManagementService) GetAllSecrets(_ context.Context, _ esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	// TO be implemented
	return nil, fmt.Errorf("GetAllSecrets not implemented")
}

func (vms *VaultManagementService) GetSecret(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	if utils.IsNil(vms.Client) {
		return nil, fmt.Errorf(errUninitalizedOracleProvider)
	}

	sec, err := vms.Client.GetSecretBundleByName(ctx, secrets.GetSecretBundleByNameRequest{
		VaultId:    &vms.vault,
		SecretName: &ref.Key,
		Stage:      secrets.GetSecretBundleByNameStageEnum(ref.Version),
	})
	if err != nil {
		return nil, util.SanitizeErr(err)
	}

	bt, ok := sec.SecretBundleContent.(secrets.Base64SecretBundleContentDetails)
	if !ok {
		return nil, fmt.Errorf(errUnexpectedContent)
	}

	payload, err := base64.StdEncoding.DecodeString(*bt.Content)
	if err != nil {
		return nil, err
	}

	if ref.Property == "" {
		return payload, nil
	}

	val := gjson.Get(string(payload), ref.Property)

	if !val.Exists() {
		return nil, fmt.Errorf(errMissingKey, ref.Key)
	}

	return []byte(val.String()), nil
}

func (vms *VaultManagementService) GetSecretMap(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) (map[string][]byte, error) {
	data, err := vms.GetSecret(ctx, ref)
	if err != nil {
		return nil, err
	}
	kv := make(map[string]string)
	err = json.Unmarshal(data, &kv)
	if err != nil {
		return nil, fmt.Errorf(errJSONSecretUnmarshal, err)
	}
	secretData := make(map[string][]byte)
	for k, v := range kv {
		secretData[k] = []byte(v)
	}
	return secretData, nil
}

// Capabilities return the provider supported capabilities (ReadOnly, WriteOnly, ReadWrite).
func (vms *VaultManagementService) Capabilities() esv1beta1.SecretStoreCapabilities {
	return esv1beta1.SecretStoreReadOnly
}

// NewClient constructs a new secrets client based on the provided store.
func (vms *VaultManagementService) NewClient(ctx context.Context, store esv1beta1.GenericStore, kube kclient.Client, namespace string) (esv1beta1.SecretsClient, error) {
	storeSpec := store.GetSpec()
	oracleSpec := storeSpec.Provider.Oracle

	if oracleSpec.Vault == "" {
		return nil, fmt.Errorf(errMissingVault)
	}

	if oracleSpec.Region == "" {
		return nil, fmt.Errorf(errMissingRegion)
	}

	var (
		err                   error
		configurationProvider common.ConfigurationProvider
	)

	if oracleSpec.PrincipalType == esv1beta1.WorkloadPrincipal {
		defer vms.workloadIdentityMutex.Unlock()
		vms.workloadIdentityMutex.Lock()
		// OCI SDK requires specific environment variables for workload identity.
		if err := os.Setenv(auth.ResourcePrincipalVersionEnvVar, auth.ResourcePrincipalVersion2_2); err != nil {
			return nil, fmt.Errorf("unable to set OCI SDK environment variable %s: %w", auth.ResourcePrincipalVersionEnvVar, err)
		}
		if err := os.Setenv(auth.ResourcePrincipalRegionEnvVar, oracleSpec.Region); err != nil {
			return nil, fmt.Errorf("unable to set OCI SDK environment variable %s: %w", auth.ResourcePrincipalRegionEnvVar, err)
		}
		configurationProvider, err = auth.OkeWorkloadIdentityConfigurationProvider()
		if err := os.Unsetenv(auth.ResourcePrincipalVersionEnvVar); err != nil {
			return nil, fmt.Errorf("unabled to unset OCI SDK environment variable %s: %w", auth.ResourcePrincipalVersionEnvVar, err)
		}
		if err := os.Unsetenv(auth.ResourcePrincipalRegionEnvVar); err != nil {
			return nil, fmt.Errorf("unabled to unset OCI SDK environment variable %s: %w", auth.ResourcePrincipalRegionEnvVar, err)
		}
		if err != nil {
			return nil, err
		}
	} else if oracleSpec.PrincipalType == esv1beta1.InstancePrincipal || oracleSpec.Auth == nil {
		configurationProvider, err = auth.InstancePrincipalConfigurationProvider()
	} else {
		configurationProvider, err = getUserAuthConfigurationProvider(ctx, kube, oracleSpec, namespace, store.GetObjectKind().GroupVersionKind().Kind, oracleSpec.Region)
	}
	if err != nil {
		return nil, fmt.Errorf(errOracleClient, err)
	}

	secretManagementService, err := secrets.NewSecretsClientWithConfigurationProvider(configurationProvider)
	if err != nil {
		return nil, fmt.Errorf(errOracleClient, err)
	}

	secretManagementService.SetRegion(oracleSpec.Region)

	kmsVaultClient, err := keymanagement.NewKmsVaultClientWithConfigurationProvider(configurationProvider)
	if err != nil {
		return nil, fmt.Errorf(errOracleClient, err)
	}

	kmsVaultClient.SetRegion(oracleSpec.Region)

	if storeSpec.RetrySettings != nil {
		opts := []common.RetryPolicyOption{common.WithShouldRetryOperation(common.DefaultShouldRetryOperation)}

		if mr := storeSpec.RetrySettings.MaxRetries; mr != nil {
			opts = append(opts, common.WithMaximumNumberAttempts(uint(*mr)))
		}

		if ri := storeSpec.RetrySettings.RetryInterval; ri != nil {
			i, err := time.ParseDuration(*storeSpec.RetrySettings.RetryInterval)
			if err != nil {
				return nil, fmt.Errorf(errOracleClient, err)
			}
			opts = append(opts, common.WithFixedBackoff(i))
		}

		customRetryPolicy := common.NewRetryPolicyWithOptions(opts...)

		secretManagementService.SetCustomClientConfiguration(common.CustomClientConfiguration{
			RetryPolicy: &customRetryPolicy,
		})

		kmsVaultClient.SetCustomClientConfiguration(common.CustomClientConfiguration{
			RetryPolicy: &customRetryPolicy,
		})
	}

	return &VaultManagementService{
		Client:         secretManagementService,
		KmsVaultClient: kmsVaultClient,
		vault:          oracleSpec.Vault,
	}, nil
}

func getSecretData(ctx context.Context, kube kclient.Client, namespace, storeKind string, secretRef esmeta.SecretKeySelector) (string, error) {
	if secretRef.Name == "" {
		return "", fmt.Errorf(errORACLECredSecretName)
	}

	objectKey := types.NamespacedName{
		Name:      secretRef.Name,
		Namespace: namespace,
	}

	// only ClusterStore is allowed to set namespace (and then it's required)
	if storeKind == esv1beta1.ClusterSecretStoreKind {
		if secretRef.Namespace == nil {
			return "", fmt.Errorf(errInvalidClusterStoreMissingSKNamespace)
		}
		objectKey.Namespace = *secretRef.Namespace
	}

	secret := corev1.Secret{}
	err := kube.Get(ctx, objectKey, &secret)
	if err != nil {
		return "", fmt.Errorf(errFetchSAKSecret, err)
	}

	return string(secret.Data[secretRef.Key]), nil
}

func getUserAuthConfigurationProvider(ctx context.Context, kube kclient.Client, store *esv1beta1.OracleProvider, namespace, storeKind, region string) (common.ConfigurationProvider, error) {
	privateKey, err := getSecretData(ctx, kube, namespace, storeKind, store.Auth.SecretRef.PrivateKey)
	if err != nil {
		return nil, err
	}
	if privateKey == "" {
		return nil, fmt.Errorf(errMissingPK)
	}

	fingerprint, err := getSecretData(ctx, kube, namespace, storeKind, store.Auth.SecretRef.Fingerprint)
	if err != nil {
		return nil, err
	}
	if fingerprint == "" {
		return nil, fmt.Errorf(errMissingFingerprint)
	}

	if store.Auth.User == "" {
		return nil, fmt.Errorf(errMissingUser)
	}

	if store.Auth.Tenancy == "" {
		return nil, fmt.Errorf(errMissingTenancy)
	}

	return common.NewRawConfigurationProvider(store.Auth.Tenancy, store.Auth.User, region, fingerprint, privateKey, nil), nil
}

func (vms *VaultManagementService) Close(_ context.Context) error {
	return nil
}

func (vms *VaultManagementService) Validate() (esv1beta1.ValidationResult, error) {
	_, err := vms.KmsVaultClient.GetVault(
		context.Background(), keymanagement.GetVaultRequest{
			VaultId: &vms.vault,
		},
	)
	if err != nil {
		failure, ok := common.IsServiceError(err)
		if ok {
			code := failure.GetCode()
			switch code {
			case "NotAuthenticated":
				return esv1beta1.ValidationResultError, err
			case "NotAuthorizedOrNotFound":
				// User authentication was successful, but user might not have a permission like:
				//
				// Allow group external_secrets to read vaults in tenancy
				//
				// Which is fine, because to read secrets we only need:
				//
				// Allow group external_secrets to read secret-family in tenancy
				//
				// But we can't test for this permission without knowing the name of a secret
				return esv1beta1.ValidationResultUnknown, err
			default:
				return esv1beta1.ValidationResultError, err
			}
		} else {
			return esv1beta1.ValidationResultError, err
		}
	}

	return esv1beta1.ValidationResultReady, nil
}

func (vms *VaultManagementService) ValidateStore(store esv1beta1.GenericStore) error {
	storeSpec := store.GetSpec()
	oracleSpec := storeSpec.Provider.Oracle

	vault := oracleSpec.Vault
	if vault == "" {
		return fmt.Errorf("vault cannot be empty")
	}

	region := oracleSpec.Region
	if region == "" {
		return fmt.Errorf("region cannot be empty")
	}

	auth := oracleSpec.Auth
	if auth == nil {
		return nil
	}

	user := oracleSpec.Auth.User
	if user == "" {
		return fmt.Errorf("user cannot be empty")
	}

	tenant := oracleSpec.Auth.Tenancy
	if tenant == "" {
		return fmt.Errorf("tenant cannot be empty")
	}
	privateKey := oracleSpec.Auth.SecretRef.PrivateKey

	if privateKey.Name == "" {
		return fmt.Errorf("privateKey.name cannot be empty")
	}

	if privateKey.Key == "" {
		return fmt.Errorf("privateKey.key cannot be empty")
	}

	err := utils.ValidateSecretSelector(store, privateKey)
	if err != nil {
		return err
	}

	fingerprint := oracleSpec.Auth.SecretRef.Fingerprint

	if fingerprint.Name == "" {
		return fmt.Errorf("fingerprint.name cannot be empty")
	}

	if fingerprint.Key == "" {
		return fmt.Errorf("fingerprint.key cannot be empty")
	}

	err = utils.ValidateSecretSelector(store, fingerprint)
	if err != nil {
		return err
	}

	return nil
}

func init() {
	esv1beta1.Register(&VaultManagementService{}, &esv1beta1.SecretStoreProvider{
		Oracle: &esv1beta1.OracleProvider{},
	})
}
