# On-prem bootstrap: a kind cluster on this machine, Argo CD in it, and one
# root Application. From then on Argo CD deploys everything under
# deploy/argocd/ from git; Terraform only owns the cluster and Argo CD.

resource "kind_cluster" "this" {
  name            = var.cluster_name
  node_image      = var.node_image
  kubeconfig_path = abspath("${path.root}/${var.kubeconfig_path}")
  wait_for_ready  = true
}

provider "helm" {
  kubernetes = {
    host                   = kind_cluster.this.endpoint
    client_certificate     = kind_cluster.this.client_certificate
    client_key             = kind_cluster.this.client_key
    cluster_ca_certificate = kind_cluster.this.cluster_ca_certificate
  }
}

resource "helm_release" "argocd" {
  name             = "argocd"
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = var.argocd_chart_version
  namespace        = "argocd"
  create_namespace = true
  wait             = true
  timeout          = 600

  values = [yamlencode({
    # Reached via kubectl port-forward on a laptop, so plain HTTP is enough.
    configs = { params = { "server.insecure" = true } }
    # Not needed for a single-user local setup.
    dex           = { enabled = false }
    notifications = { enabled = false }
    # Small requests so Argo CD fits next to the vLLM replicas in 18 GB.
    controller     = { resources = { requests = { cpu = "100m", memory = "256Mi" } } }
    repoServer     = { resources = { requests = { cpu = "50m", memory = "128Mi" } } }
    server         = { resources = { requests = { cpu = "50m", memory = "64Mi" } } }
    redis          = { resources = { requests = { cpu = "50m", memory = "64Mi" } } }
    applicationSet = { resources = { requests = { cpu = "50m", memory = "64Mi" } } }
  })]
}

# Installed through the argocd-apps chart rather than a kubernetes_manifest
# resource: kubernetes_manifest needs the Application CRD to exist at plan
# time, which it doesn't until Argo CD is installed in this same apply.
resource "helm_release" "root_app" {
  name       = "root"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argocd-apps"
  version    = var.argocd_apps_chart_version
  namespace  = "argocd"

  values = [yamlencode({
    applications = {
      root = {
        namespace = "argocd"
        project   = "default"
        source = {
          repoURL        = var.repo_url
          targetRevision = var.target_revision
          path           = "deploy/argocd"
        }
        destination = {
          server    = "https://kubernetes.default.svc"
          namespace = "argocd"
        }
        syncPolicy = {
          automated = { prune = true, selfHeal = true }
        }
      }
    }
  })]

  depends_on = [helm_release.argocd]
}
