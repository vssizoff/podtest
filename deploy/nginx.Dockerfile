FROM docker.io/library/node:26-alpine3.23 AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
ARG NPM_CONFIG_REGISTRY=https://registry.npmjs.org
RUN npm ci --no-audit
COPY frontend/ ./
RUN npm run build

FROM docker.io/library/nginx:1.29.0-alpine3.22
COPY --from=build /app/dist /usr/share/nginx/html
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
COPY deploy/proxy.inc /etc/nginx/proxy.inc
COPY --chmod=755 deploy/nginx-resolver.sh /docker-entrypoint.d/40-podman-resolver.sh
