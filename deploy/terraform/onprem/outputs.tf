output "kubeconfig_path" {
  description = "Use with: kubectl --kubeconfig <path> ..."
  value       = kind_cluster.this.kubeconfig_path
}

output "argocd_ui" {
  description = "How to open the Argo CD UI."
  value       = "kubectl --kubeconfig ${kind_cluster.this.kubeconfig_path} -n argocd port-forward svc/argocd-server 8443:80, then http://localhost:8443 (user admin; password: kubectl ... -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)"
}
