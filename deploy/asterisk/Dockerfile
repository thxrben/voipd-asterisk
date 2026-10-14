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
# /var/run is a symlink to /run in the final image; BuildKit refuses to copy
# a directory over it. (Separate step so the compile above stays cached.)
RUN if [ -d /out/var/run ]; then \
        mkdir -p /out/run && cp -a /out/var/run/. /out/run/ && rm -rf /out/var/run; \
    fi

# German voice prompts, rendered once at build time with Piper TTS (offline,
# "Thorsten" voice); nothing TTS-related ends up in the final image.
FROM debian:${DEBIAN} AS prompts
ARG PIPER_RELEASE=2023.11.14-2
ARG PIPER_SHA256_X86_64=a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992
ARG PIPER_SHA256_AARCH64=fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb
ARG VOICE_URL=https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/de/de_DE/thorsten/medium/de_DE-thorsten-medium
ARG VOICE_SHA256=7e64762d8e5118bb578f2eea6207e1a35a8e0c30595010b666f983fc87bb7819
ARG VOICE_JSON_SHA256=974adee790533adb273a1ac88f49027d2a1b8f0f2cf4905954a4791e79264e85
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl sox \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; arch=$(uname -m); \
    case "$arch" in x86_64) sum=$PIPER_SHA256_X86_64 ;; aarch64) sum=$PIPER_SHA256_AARCH64 ;; \
        *) echo "no piper build for $arch"; exit 1 ;; esac; \
    curl -fsSL -o /tmp/piper.tgz "https://github.com/rhasspy/piper/releases/download/${PIPER_RELEASE}/piper_linux_${arch}.tar.gz"; \
    echo "$sum  /tmp/piper.tgz" | sha256sum -c -; \
    mkdir -p /opt && tar xzf /tmp/piper.tgz -C /opt && rm /tmp/piper.tgz; \
    curl -fsSL -o /opt/piper/voice.onnx "${VOICE_URL}.onnx"; \
    curl -fsSL -o /opt/piper/voice.onnx.json "${VOICE_URL}.onnx.json"; \
    echo "$VOICE_SHA256  /opt/piper/voice.onnx" | sha256sum -c -; \
    echo "$VOICE_JSON_SHA256  /opt/piper/voice.onnx.json" | sha256sum -c -
COPY prompts/ /prompts/
RUN sh /prompts/render.sh /prompts/de.txt /out/sounds/voipd

FROM debian:${DEBIAN}
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates libedit2 libjansson4 libxml2 libsqlite3-0 libuuid1 \
        libssl3t64 libsrtp2-1 libunbound8 libcurl4t64 gettext-base tini \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r asterisk && useradd -r -g asterisk -d /var/lib/asterisk asterisk
COPY --from=build /out/ /
COPY --from=prompts /out/sounds/ /var/lib/asterisk/sounds/
COPY conf/ /usr/share/voipd/asterisk-conf/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh \
    && mkdir -p /etc/asterisk/voipd /var/spool/asterisk /var/log/asterisk /var/run/asterisk \
    && chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/spool/asterisk /var/log/asterisk /var/run/asterisk
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
CMD ["asterisk", "-f", "-U", "asterisk", "-G", "asterisk", "-vvv"]
