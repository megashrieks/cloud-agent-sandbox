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
RUN chown mitmproxy:mitmproxy /addons/inject.py

USER mitmproxy
EXPOSE 8080

CMD ["mitmdump", "--listen-port", "8080", "-s", "/addons/inject.py", "--set", "confdir=/home/mitmproxy/.mitmproxy"]