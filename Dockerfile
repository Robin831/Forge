FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    git \
    openssh-client

RUN addgroup -S forge && adduser -S forge -G forge

COPY --chown=forge:forge forge /usr/local/bin/forge

USER forge

ENTRYPOINT ["forge"]
