FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kiko-backend .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kiko-backend /kiko-backend

ENV KIKO_LISTEN_ADDR=":8080"
ENV CARDANO_NETWORK="preprod"
ENV X402_FACILITATOR_URL=""
ENV MAPBOX_PERMANENT="false"
ENV KOIOS_URL=""
ENV KIKO_SERVER_MNEMONIC=""
ENV MAPBOX_ACCESS_TOKEN=""
ENV KOIOS_API_TOKEN=""
ENV BSV_FACILITATOR_URL=""
ENV BSV_ARC_URL=""
ENV BSV_ARC_API_KEY=""

EXPOSE 8080
ENTRYPOINT ["/kiko-backend"]
