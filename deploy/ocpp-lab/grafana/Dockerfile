# hyde/lab: bake provisioning + dashboards into the image instead of
# bind-mounting them. Dokploy re-clones the repo on every deploy, which breaks
# bind mounts under any container it doesn't recreate; baking sidesteps that and
# forces a recreate whenever provisioning changes. Dashboards live under
# /etc/grafana (not /var/lib/grafana, which can be a volume mount point).
FROM grafana/grafana:11.2.0
# Business Calendar (v4.x = Grafana 11/12): sessions-as-events mini calendar on
# HYDE · Overview; drag-selecting days sets the dashboard time range. Installed
# at build time — the image's anonymous /var/lib/grafana volume is seeded from
# image content on every (re)create, so the plugin survives Dokploy redeploys.
RUN grafana-cli plugins install marcusolsson-calendar-panel 4.2.4
COPY provisioning /etc/grafana/provisioning
COPY dashboards /etc/grafana/dashboards
