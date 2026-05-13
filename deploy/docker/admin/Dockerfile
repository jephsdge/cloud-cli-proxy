FROM node:24-alpine AS build

WORKDIR /app
ARG NPM_CONFIG_REGISTRY=https://registry.npmmirror.com
ENV npm_config_registry=${NPM_CONFIG_REGISTRY}
COPY web/admin/package.json web/admin/pnpm-lock.yaml ./
RUN npm install -g "pnpm@10.30.3" --registry="${NPM_CONFIG_REGISTRY}" \
    && pnpm install --frozen-lockfile
COPY web/admin/ .
RUN pnpm build

FROM nginx:alpine
COPY deploy/docker/admin/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
