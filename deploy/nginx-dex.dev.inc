location ^~ /dex/ {
    limit_req zone=auth burst=12 nodelay;
    set $podtest_dex dex:5556;
    proxy_pass http://$podtest_dex;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-For $remote_addr;
    # Dex's login template includes inline JavaScript. Keep the app's CSP
    # unchanged and apply the different policy only to Dex responses.
    add_header Content-Security-Policy "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; form-action 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" always;
    add_header X-Content-Type-Options nosniff always;
    add_header X-Frame-Options DENY always;
    add_header Referrer-Policy no-referrer always;
}
