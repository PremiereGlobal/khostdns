FROM alpine:3.9

RUN apk update && \
    apk -Uuv add dumb-init ca-certificates && \
    rm /var/cache/apk/*
COPY run.sh /run.sh
RUN touch env.sh
COPY build/khostdns /khostdns

ENTRYPOINT ["/run.sh"]
CMD ["/khostdns"]

