FROM docker.io/library/golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /backend ./cmd/backend

FROM docker.io/library/node:26-alpine3.23 AS math
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --omit=dev
COPY deploy/math.cjs ./

FROM docker.io/library/node:26-alpine3.23
COPY --from=build /backend /usr/local/bin/backend
COPY --from=math /app/math.cjs /app/math.cjs
COPY --from=math /app/node_modules/katex /app/node_modules/katex
ENV MATH_RENDERER_SCRIPT=/app/math.cjs
ENTRYPOINT ["backend"]
