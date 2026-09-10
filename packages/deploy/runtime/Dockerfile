# syntax=docker/dockerfile:1.7
ARG TEXLIVE_IMAGE=__TEXLIVE_IMAGE__
FROM ${TEXLIVE_IMAGE} AS tex
USER root
ARG TEXLIVE_REPOSITORY=__TEXLIVE_REPOSITORY__
ARG TEXLIVE_YEAR=__TEXLIVE_YEAR__
ARG PROFILE=__PROFILE__
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    case "$PROFILE" in \
      slim) packages='libwww-perl liblwp-protocol-https-perl' ;; \
      full) packages='' ;; \
      *) exit 1 ;; \
    esac \
    && rm -f /etc/apt/apt.conf.d/docker-clean \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates fontconfig \
       fonts-dejavu-core fonts-liberation fonts-inconsolata fonts-noto-cjk \
       fonts-crosextra-carlito fonts-crosextra-caladea $packages
# Release Apt cache locks before the long network-bound TeX installation.
COPY packages.txt install-texlive.sh /usr/local/lib/latexmk/
RUN sh /usr/local/lib/latexmk/install-texlive.sh

# Editing font aliases only rebuilds this stage and the inexpensive final layer.
FROM tex AS fonts
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends python3-fonttools
COPY rename-compat-fonts.py /tmp/rename-compat-fonts.py
RUN mkdir /font-build /font-output \
    && find /usr/local/texlive -path '*/fonts/opentype/public/tex-gyre/*' -name '*.otf' -exec cp {} /font-build/ \; \
    && python3 /tmp/rename-compat-fonts.py /font-build \
    && cp /font-build/Latexmk-*.otf /font-output/

FROM tex AS runtime
COPY --from=fonts /font-output/ /usr/local/share/fonts/latexmk/
COPY smoke.sh font-smoke.tex font-inventory.sh /usr/local/lib/latexmk/
RUN fc-cache -f \
    && sh /usr/local/lib/latexmk/font-inventory.sh /usr/local/lib/latexmk \
    && groupadd --gid 10001 latexmk \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin latexmk \
    && install --directory --owner=latexmk --group=latexmk --mode=0700 /var/lib/latexmk
ENV PORT=8080 \
    LATEXMK_IMAGE_PROFILE=__IMAGE_PROFILE__ \
    LATEXMK_ENGINES=__ENGINES__ \
    LATEXMK_TEMP_DIR=/tmp \
    HOME=/home/latexmk \
    LANG=C.UTF-8
USER 10001:10001
RUN sh /usr/local/lib/latexmk/smoke.sh
WORKDIR /home/latexmk
