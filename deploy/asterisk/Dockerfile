# Asterisk as a SIP/media engine for voipd.
#
# Built from source so we control the module set: PJSIP with the unbound
# resolver (needed for NAPTR, e.g. Deutsche Telekom), SRTP, ARI with external
# MWI, and none of the legacy channel drivers or dialplan apps we don't use.
ARG DEBIAN=trixie-slim

FROM debian:${DEBIAN} AS build
ARG ASTERISK_VERSION=22.11.0
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl bzip2 patch pkg-config \
        libedit-dev libjansson-dev libxml2-dev libsqlite3-dev uuid-dev \
        libssl-dev libsrtp2-dev libunbound-dev libcurl4-openssl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSL "https://downloads.asterisk.org/pub/telephony/asterisk/releases/asterisk-${ASTERISK_VERSION}.tar.gz" \
        | tar xz --strip-components=1
RUN ./configure --with-pjproject-bundled --with-jansson --with-srtp --with-unbound \
        --without-x11 --without-gtk2 --prefix=/usr --sysconfdir=/etc --localstatedir=/var \
    && make menuselect.makeopts \
    && menuselect/menuselect \
        --disable BUILD_NATIVE \
        --enable res_mwi_external --enable res_ari_mailboxes --enable res_mwi_external_ami \
        --enable res_resolver_unbound \
        --disable app_voicemail --disable app_minivm \
        --disable chan_iax2 \
        --disable chan_dahdi --disable cdr_csv --disable cel_custom \
        --disable-category MENUSELECT_CORE_SOUNDS --disable-category MENUSELECT_MOH \
        --disable-category MENUSELECT_EXTRA_SOUNDS \
        menuselect.makeopts \
    && make -j"$(nproc)" \
    && make install DESTDIR=/out \
    && rm -rf /out/usr/include

FROM debian:${DEBIAN}
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates libedit2 libjansson4 libxml2 libsqlite3-0 libuuid1 \
        libssl3t64 libsrtp2-1 libunbound8 libcurl4t64 gettext-base tini \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r asterisk && useradd -r -g asterisk -d /var/lib/asterisk asterisk
COPY --from=build /out/ /
COPY conf/ /usr/share/voipd/asterisk-conf/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh \
    && mkdir -p /etc/asterisk/voipd /var/spool/asterisk /var/log/asterisk /var/run/asterisk \
    && chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/spool/asterisk /var/log/asterisk /var/run/asterisk
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
CMD ["asterisk", "-f", "-U", "asterisk", "-G", "asterisk", "-vvv"]
