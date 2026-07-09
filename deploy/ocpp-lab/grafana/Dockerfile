# hyde/lab: bake provisioning + dashboards into the image instead of
# bind-mounting them. Dokploy re-clones the repo on every deploy, which breaks
# bind mounts under any container it doesn't recreate; baking sidesteps that and
# forces a recreate whenever provisioning changes. Dashboards live under
# /etc/grafana (not /var/lib/grafana, which can be a volume mount point).
FROM grafana/grafana:11.2.0
COPY provisioning /etc/grafana/provisioning
COPY dashboards /etc/grafana/dashboards
