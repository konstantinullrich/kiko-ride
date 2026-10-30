FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kiko-backend .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kiko-backend /kiko-backend

ENV KIKO_LISTEN_ADDR=":8080"
ENV CARDANO_NETWORK="preprod"
ENV X402_FACILITATOR_URL=""
ENV KIKO_FIRST_ADDRESS_INDEX="0"
ENV MAPBOX_PERMANENT="false"
ENV OGMIOS_URL=""
ENV KUPO_URL=""
ENV BLOCKFROST_URL=""
ENV KIKO_SERVER_MNEMONIC=""
ENV MAPBOX_ACCESS_TOKEN=""
ENV BLOCKFROST_PROJECT_ID=""

EXPOSE 8080
ENTRYPOINT ["/kiko-backend"]
