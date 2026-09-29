#!/bin/sh
# The image's welcome page.
rm -f /etc/nginx/conf.d/default.conf

# Before `make cert` there is no certificate: serve only :80 for the ACME
# challenge, `make cert` restarts nginx afterwards.
if [ ! -f "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ]; then
    echo "$0: no certificate for $DOMAIN yet, HTTPS is off"
    rm -f /etc/nginx/conf.d/https.conf
fi

# certbot renews in its own container; pick the new files up.
(while sleep 12h; do nginx -s reload; done) &
