# vault-gcp-gke-wi-auth

This utility is intended to run as a container in a GKE cluster. It
exchanges a Kubernetes service account token for GCP credentials, and then uses
those credentials to authenticate with Vault and get a Vault token.

The provided example deploys the container as a Kuberenetes job, which will run
only once.

To operate successfully, a number of prerequisites must be met:
- GCP IAM mapping that associates a Kubernetes service account and namespace
with a Google Service Account
- Kubernetes service account associated with this container that is annotated in
such a way that it assumes the target Google Service Account identity
- A Kubernetes role and rolebinding that grants the Kubernetes service account
the ability to read service accounts and generate service account tokens
- A healthy Vault cluster with an appropriately configured GCP auth method

These requirements and others are fully represented in the provided Terraform
example, which should provide the necessary guidance to deploy this tool. Note
that the included Terraform example in `deploy.tf` is not intended for direct
use and is provided mainly to illustrate these infrastructure requirements.

This repository demonstrates a container build and deployment workflow
orchestrated by GitHub actions.