# From what `make build` leaves in build/, the context. No base image to pull.
FROM scratch
ARG ARCH
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY olcrtc-${ARCH} /olcrtc
ENTRYPOINT ["/olcrtc", "/config.yaml"]
