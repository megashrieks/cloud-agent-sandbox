# MITM proxy image for sandbox git credential injection.
#
# mitmdump creates its certificate authority under the configured confdir on
# startup. We pin that directory so the orchestrator can extract:
#   /home/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem
# and install it in sandboxes that are configured to use this proxy.
FROM mitmproxy/mitmproxy:latest

USER root
RUN mkdir -p /addons /home/mitmproxy/.mitmproxy \
    && chown -R mitmproxy:mitmproxy /addons /home/mitmproxy

COPY addons/inject.py /addons/inject.py
COPY start.sh /usr/local/bin/start.sh
RUN chown mitmproxy:mitmproxy /addons/inject.py \
    && chmod +x /usr/local/bin/start.sh

USER mitmproxy
EXPOSE 8080

# Bypass the base image's root-only usermod entrypoint; run mitmdump directly.
ENTRYPOINT ["/usr/local/bin/start.sh"]
CMD []