FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/betterleaks/betterleaks"
RUN apk add --no-cache bash git openssh-client
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/betterleaks /usr/bin/betterleaks
RUN git config --global --add safe.directory '*'
ENTRYPOINT ["betterleaks"]
