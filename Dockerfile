FROM gcr.io/distroless/static-debian11:nonroot
ENTRYPOINT ["/baton-grafana"]
COPY baton-grafana /