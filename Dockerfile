FROM ubuntu:latest as build-base
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update
RUN apt-get install -qq build-essential ca-certificates pkg-config


FROM build-base as build-go
ARG GOLANG
WORKDIR /opt
ADD $GOLANG .
ENV PATH="/opt/go/bin:${PATH}"


FROM build-base AS lxc
ARG LXC_SRC
RUN apt-get install -qq --no-install-recommends --yes \
    libapparmor-dev libbtrfs-dev libc6-dev libcap-dev \
    libdevmapper-dev libseccomp-dev
WORKDIR /tmp/build
COPY $LXC_SRC .
RUN tar -xf $(basename $LXC_SRC) --strip-components=1 --no-same-owner
RUN ./configure --enable-bash=no --enable-seccomp=yes \
    --enable-capabilities=yes --enable-apparmor=yes \
    --enable-tools=no --enable-commands=no \
    --enable-static=no --enable-examples=no \
    --enable-doc=no --enable-api-docs=no
RUN make install


FROM build-go AS lxcri
ARG LXCRI_SRC
COPY --from=lxc /usr/local/ /usr/local/
RUN apt-get update
RUN apt-get install -qq --no-install-recommends --yes \
    libapparmor1 libbtrfs0 libcap2 libdevmapper1.02.1 libseccomp2
WORKDIR /tmp/build
COPY $LXCRI_SRC .
RUN tar -xf $(basename $LXCRI_SRC) --strip-components=1
ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig
RUN make install
