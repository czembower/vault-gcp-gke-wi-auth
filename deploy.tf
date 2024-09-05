provider "google" {
  project = var.gcp_project_id
  region  = var.gcp_region
}

data "google_container_cluster" "gke" {
  name     = var.gke_cluster_name
  location = var.gcp_region
}

data "google_client_config" "current" {}

provider "kubernetes" {
  host                   = "https://${data.google_container_cluster.gke.endpoint}"
  token                  = data.google_client_config.current.access_token
  cluster_ca_certificate = base64decode(data.google_container_cluster.gke1.master_auth[0].cluster_ca_certificate)
}

variable "gke_cluster_name" {
  type        = string
  description = "GKE cluster name"
}

variable "container_registry" {
  type        = string
  description = "Container registry to use for the vggwa image"
}

variable "github_api_user" {
  type        = string
  description = "GitHub API username"
}

variable "github_api_token" {
  type        = string
  description = "GitHub API token (PAT with read:packages scope)"
}

variable "gcp_project_id" {
  type        = string
  description = "GCP project ID"
}

variable "gcp_region" {
  type        = string
  description = "GCP region"
}

variable "vault_address" {
  type        = string
  description = "Vault cluster API address"
}

variable "vault_gcp_auth_mount_path" {
  type        = string
  description = "Vault GCP auth mount path"
  default     = "gcp"
}

resource "kubernetes_namespace" "vault" {
  metadata {
    annotations = {
      name = "vault"
    }
    name = "vault"
  }
}

resource "google_service_account" "gke_vggwa_service_account" {
  account_id   = "vggwa-wi-sa"
  display_name = "GKE vggwa Service Account"
}

resource "google_project_iam_member" "gke_vggwa_workload_identity" {
  project = var.gcp_project_id
  role    = "roles/iam.workloadIdentityUser"
  member  = "serviceAccount:${var.gcp_project_id}.svc.id.goog[vault/vggwa]"
}

resource "kubernetes_service_account_v1" "vggwa" {
  metadata {
    name      = "vggwa"
    namespace = kubernetes_namespace.vault.metadata[0].name
    annotations = {
      "iam.gke.io/gcp-service-account" = "${google_service_account.gke_vggwa_service_account.email}"
    }
  }
}

resource "kubernetes_secret_v1" "ghcr" {
  metadata {
    name      = "ghcr-registry"
    namespace = kubernetes_namespace.vault.metadata[0].name
  }

  data = {
    ".dockerconfigjson" = "${data.template_file.docker_config_script.rendered}"
  }

  type = "kubernetes.io/dockerconfigjson"
}


data "template_file" "docker_config_script" {
  template = file("${path.module}/resources/config.json")
  vars = {
    docker-username = var.github_api_user
    docker-password = var.github_api_token
    docker-server   = "ghcr.io"
    auth            = base64encode("${var.github_api_user}:${var.github_api_token}")
  }
}

resource "kubernetes_role_v1" "vggwa" {
  metadata {
    name      = "vggwa"
    namespace = kubernetes_namespace.vault.metadata[0].name
  }

  rule {
    api_groups = [""]
    resources  = ["serviceaccounts"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = [""]
    resources  = ["serviceaccounts/token"]
    verbs      = ["create", "get", "list", "watch"]
  }
}

resource "kubernetes_role_binding_v1" "vggwa" {
  metadata {
    name      = "vggwa"
    namespace = kubernetes_namespace.vault.metadata[0].name
  }
  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = kubernetes_role_v1.vggwa.metadata[0].name
  }
  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account_v1.vggwa.metadata[0].name
    namespace = kubernetes_namespace.vault.metadata[0].name
    api_group = ""
  }
}

resource "kubernetes_job_v1" "vggwa" {

  metadata {
    name      = "vggwa"
    namespace = kubernetes_namespace.vault.metadata[0].name
  }
  spec {
    template {
      metadata {}
      spec {
        service_account_name = kubernetes_service_account_v1.vggwa.metadata[0].name
        image_pull_secrets {
          name = kubernetes_secret_v1.ghcr.metadata[0].name
        }
        container {
          name              = "vggwa"
          image             = "${var.container_registry}/vggwa:latest"
          image_pull_policy = "Always"
          resources {
            requests = {
              cpu               = "1"
              memory            = "1Gi"
              ephemeral-storage = "1Gi"
            }
          }
          env {
            name  = "VAULT_ROLE"
            value = "client"
          }
          env {
            name  = "VAULT_ADDR"
            value = var.vault_address
          }
          env {
            name  = "VAULT_GCP_AUTH_MOUNT_PATH"
            value = var.vault_gcp_auth_mount_path
          }
          env {
            name  = "VAULT_SKIP_VERIFY"
            value = "true"
          }
        }
      }
    }
  }
  lifecycle {
    ignore_changes = [
      spec,
      metadata
    ]
  }
}
