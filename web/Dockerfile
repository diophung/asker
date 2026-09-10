# Build stage: compile the Vite app.
FROM node:26-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
# Build-time configuration (baked into the static bundle).
ARG VITE_KEYCLOAK_URL=http://localhost:8081
ARG VITE_API_URL=http://localhost:8080
ENV VITE_KEYCLOAK_URL=${VITE_KEYCLOAK_URL} \
    VITE_API_URL=${VITE_API_URL}
RUN npm run build

# Serve stage: static files behind nginx with SPA fallback.
FROM nginx:alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
# Included per-location by nginx.conf; nginx fails to start if it is missing.
COPY security-headers.conf /etc/nginx/snippets/security-headers.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
    CMD wget -q -O /dev/null http://127.0.0.1/healthz || exit 1
