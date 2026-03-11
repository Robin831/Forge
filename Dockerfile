FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    git \
    openssh-client

COPY forge /usr/local/bin/forge

ENTRYPOINT ["forge"]
