package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/golang-jwt/jwt"
	"github.com/hashicorp/vault-client-go"
	"github.com/hashicorp/vault-client-go/schema"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
	stsv1 "google.golang.org/api/sts/v1"
	v12 "k8s.io/api/authentication/v1"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
)

type GCPTokenExchangeConfig struct {
	KSAName        string
	KSANamespace   string
	GSAName        string
	GKEClusterName string
	GCPProject     string
	Region         string
	VaultRole      string
}

func getKubeClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func getSAAnnotation(ctx context.Context, namespace string, name string) (string, error) {
	clientset, err := getKubeClient()
	if err != nil {
		return "", err
	}
	saGet, err := clientset.CoreV1().ServiceAccounts(namespace).Get(ctx, name, v13.GetOptions{})
	if err != nil {
		return "", err
	}
	return saGet.GetAnnotations()["iam.gke.io/gcp-service-account"], nil
}

func tokenRequest(ctx context.Context, serviceAccountName string, namespace string, expirationSeconds int64, audiences []string) (*v12.TokenRequest, error) {
	clientset, err := getKubeClient()
	if err != nil {
		return nil, err
	}
	treq := &v12.TokenRequest{
		Spec: v12.TokenRequestSpec{
			ExpirationSeconds: pointer.Int64(expirationSeconds),
			Audiences:         audiences,
		},
	}

	req, err := clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccountName, treq, v13.CreateOptions{})
	if err != nil {
		panic(err)
	}

	return req, nil
}

// GCPTokenExchange exchanges a Kubernetes service account token for GCP credentials
func GCPTokenExchange(ctx context.Context, config GCPTokenExchangeConfig) (string, error) {

	// build the audience for the federated token
	workloadIdentityPool := fmt.Sprintf("%s.svc.id.goog", config.GCPProject)
	identityProvider := fmt.Sprintf(
		"https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
		config.GCPProject, config.Region, config.GKEClusterName,
	)
	fedTokenAudience := fmt.Sprintf("identitynamespace:%s:%s", workloadIdentityPool, identityProvider)
	fmt.Printf("Audience: %s\n", fedTokenAudience)

	// request a token from the Kubernetes API server for the desired service account
	k8sTokenRequest, err := tokenRequest(ctx, config.KSAName, config.KSANamespace, 600, []string{workloadIdentityPool})
	if err != nil {
		return "", fmt.Errorf("failed to request token: %w", err)
	}

	// exchange a Kubernetes service account token for a Google federated token
	stsService, err := stsv1.NewService(ctx, option.WithoutAuthentication())
	if err != nil {
		return "", fmt.Errorf("failed to dial Google STS: %w", err)
	}
	stsTokenResp, err := stsService.V1.Token(&stsv1.GoogleIdentityStsV1ExchangeTokenRequest{
		SubjectToken:       k8sTokenRequest.Status.Token,
		SubjectTokenType:   "urn:ietf:params:oauth:token-type:jwt",
		RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token",
		GrantType:          "urn:ietf:params:oauth:grant-type:token-exchange",
		Scope:              "https://www.googleapis.com/auth/iam",
		Audience:           fedTokenAudience,
	}).Do()
	if err != nil {
		return "", fmt.Errorf("failed to exchange k8s service account token for google federated token: %w", err)
	}
	if stsTokenResp == nil || stsTokenResp.AccessToken == "" {
		return "", fmt.Errorf("empty token response when exchanging k8s service account token for google federated token")
	}

	// exchange the Google federated token for a Google ID token
	iamService, err := iamcredentials.NewService(ctx, option.WithoutAuthentication())
	if err != nil {
		return "", fmt.Errorf("failed to dial Google IAM: %w", err)
	}
	idTokenCall := iamService.Projects.ServiceAccounts.GenerateIdToken(
		"projects/-/serviceAccounts/"+config.GSAName,
		&iamcredentials.GenerateIdTokenRequest{
			Audience:     fmt.Sprintf("https://vault/%s", config.VaultRole),
			IncludeEmail: true,
		},
	)
	idTokenCall.Header().Set("Authorization", "Bearer "+stsTokenResp.AccessToken)
	idTokenCall.Context(ctx)
	idTokenResp, err := idTokenCall.Do()
	if err != nil {
		return "", fmt.Errorf("failed to exchange Google federated token for id token: %w", err)
	}

	return idTokenResp.Token, nil
}

func buildConfig(ctx context.Context) (GCPTokenExchangeConfig, error) {
	var config GCPTokenExchangeConfig

	// get GKE cluster name
	gkeName, err := metadata.InstanceAttributeValueWithContext(ctx, "cluster-name")
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("failed to fetch GKE cluster name from instance metadata: %v", err)
	}
	config.GKEClusterName = gkeName

	// get GCP location/region
	region, err := metadata.InstanceAttributeValueWithContext(ctx, "cluster-location")
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("unable to fetch location from instance metadata: %v", err)
	}
	config.Region = region

	// get GCP project
	project, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("unable to fetch project from instance metadata: %v", err)
	}
	config.GCPProject = project

	// get Kubernetes service account token
	localSaToken, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("unable to access service account token: %v", err)
	}

	// get Kubernetes service accont namespace
	namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("unable to access service account namespace: %v", err)
	}
	config.KSANamespace = string(namespace)

	// get Kubernetes service account name
	token, _, err := new(jwt.Parser).ParseUnverified(string(localSaToken), jwt.MapClaims{})
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("error parsing service account token: %v", err)
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok {
		if kubclaims, ok := claims["kubernetes.io"].(map[string]interface{}); ok {
			saData := kubclaims["serviceaccount"].(map[string]interface{})
			config.KSAName = saData["name"].(string)
		}
	}

	// get GSA from service account annotation
	config.GSAName, err = getSAAnnotation(ctx, config.KSANamespace, config.KSAName)
	if err != nil {
		return GCPTokenExchangeConfig{}, fmt.Errorf("failed to get GSA from service account annotation: %v", err)
	}

	// get Vault role from environment variable
	config.VaultRole = os.Getenv("VAULT_ROLE")

	return config, nil
}

func gcpAuthToVault(ctx context.Context, idToken string, config GCPTokenExchangeConfig) (string, error) {
	// setup Vault client
	tlsVerify, err := strconv.ParseBool(os.Getenv("VAULT_SKIP_VERIFY"))
	if err != nil {
		return "", err
	}
	tls := vault.TLSConfiguration{}
	tls.InsecureSkipVerify = tlsVerify
	client, err := vault.New(
		vault.WithAddress(os.Getenv("VAULT_ADDR")),
		vault.WithRequestTimeout(5*time.Second),
		vault.WithRetryConfiguration(vault.RetryConfiguration{}),
		vault.WithTLS(tls),
	)
	if err != nil {
		return "", err
	}

	vaultNamespace := os.Getenv("VAULT_NAMESPACE")
	if vaultNamespace != "" {
		client.SetNamespace(vaultNamespace)
	}

	// authenticate with Vault using GCP auth method
	c, err := client.Auth.GoogleCloudLogin(ctx, schema.GoogleCloudLoginRequest{
		Jwt:  idToken,
		Role: config.VaultRole,
	},
		vault.WithMountPath(os.Getenv("VAULT_GCP_AUTH_MOUNT_PATH")),
	)
	if err != nil {
		return "", err
	}

	return c.Auth.ClientToken, nil
}

func main() {

	ctx := context.Background()

	// build configuration
	config, err := buildConfig(ctx)
	if err != nil {
		log.Fatalf("failed to build config: %v", err)
	}
	fmt.Printf("%+v\n", config)

	// exchange Kubernetes SA token for GCP ID token
	idToken, err := GCPTokenExchange(ctx, config)
	if err != nil {
		log.Fatalf("failed to exchange tokens: %v", err)
	}

	// return Vault token
	token, err := gcpAuthToVault(ctx, idToken, config)
	if err != nil {
		log.Fatalf("failed to authenticate with Vault: %v", err)
	}
	fmt.Println("Vault token:", token)
}
