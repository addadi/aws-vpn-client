# Build openvpn with musl libc
FROM alpine:latest AS ovpn-musl

RUN apk add --no-cache \
    autoconf \
    automake \
    curl \
    go \
    libcap-ng-dev \
    libnl3-dev \
    libtool \
    linux-headers \
    linux-pam-dev \
    lz4-dev \
    lzo-dev \
    make \
    openssl-dev \
    patch \
    unzip

# Patch & build OpenVPN
ARG OPENVPN_VERSION=2.6.12

RUN curl -L "https://github.com/OpenVPN/openvpn/archive/v${OPENVPN_VERSION}.zip" -o openvpn.zip \
    && unzip openvpn.zip \
    && mv "openvpn-${OPENVPN_VERSION}" openvpn

WORKDIR /
COPY "patches/openvpn-v${OPENVPN_VERSION}-aws.patch" openvpn/aws.patch

WORKDIR /openvpn
RUN patch -p1 < aws.patch \
    && autoreconf -ivf \
    && ./configure \
    && make

# Build openvpn with glibc
FROM debian:12-slim AS ovpn-glibc

RUN apt-get update \
    && apt-get --no-install-recommends -y install \
      autoconf \
      automake \
      ca-certificates \
      curl \
      libcap-ng-dev \
      liblz4-dev \
      liblzo2-dev \
      libnl-genl-3-dev \
      libpam0g-dev \
      libssl-dev \
      libtool \
      make \
      patch \
      pkg-config \
      unzip \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Patch & build OpenVPN
ARG OPENVPN_VERSION=2.6.3

RUN curl -L "https://github.com/OpenVPN/openvpn/archive/v${OPENVPN_VERSION}.zip" -o openvpn.zip \
    && unzip openvpn.zip \
    && mv "openvpn-${OPENVPN_VERSION}" openvpn

WORKDIR /
COPY "patches/openvpn-v${OPENVPN_VERSION}-aws.patch" openvpn/aws.patch

WORKDIR /openvpn
RUN patch -p1 < aws.patch \
    && autoreconf -ivf \
    && ./configure \
    && make

# Build aws-vpn-client
FROM alpine:latest

RUN apk add --no-cache \
    bash \
    go

# CGO_ENABLED=0 for static linking
ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

ARG USER=vpn
RUN adduser --disabled-password --gecos '' ${USER} \
    && mkdir -p app

WORKDIR "/home/${USER}"
USER ${USER}:${USER}

# Copy entrypoint script into the container
COPY --chown=$USER:$USER entrypoints/make.sh entrypoint.sh

# Copy openvpn binaries into the container
COPY --from=ovpn-musl --chown=$USER:$USER openvpn/src/openvpn/openvpn openvpn-musl
COPY --from=ovpn-glibc --chown=$USER:$USER openvpn/src/openvpn/openvpn openvpn-glibc

ENTRYPOINT ["/bin/bash", "entrypoint.sh"]
