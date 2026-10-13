variable "cluster_name" {
  description = "Name of the kind cluster."
  type        = string
  default     = "llm-router"
}

variable "node_image" {
  description = "Kubernetes node image; pinned so every apply gets the same version."
  type        = string
  default     = "kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f"
}

variable "kubeconfig_path" {
  description = "Where to write the cluster's kubeconfig. Kept out of ~/.kube/config so the default kubectl context is never changed."
  type        = string
  default     = "../../../bin/kubeconfig"
}

variable "repo_url" {
  description = "Git repository Argo CD deploys from."
  type        = string
  default     = "https://github.com/suchethg/llm-router.git"
}

variable "target_revision" {
  description = "Branch, tag or commit Argo CD tracks."
  type        = string
  default     = "main"
}

variable "argocd_chart_version" {
  description = "Version of the argo/argo-cd Helm chart."
  type        = string
  default     = "10.10.3" # Argo CD v3.5.5
}

variable "argocd_apps_chart_version" {
  description = "Version of the argo/argocd-apps Helm chart."
  type        = string
  default     = "2.0.6"
}
