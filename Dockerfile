ARG NAKAMA_VERSION=3.26.0

FROM heroiclabs/nakama-pluginbuilder:${NAKAMA_VERSION} AS builder

ENV GO111MODULE on
ENV CGO_ENABLED 1

WORKDIR /backend
COPY . .

RUN go build --trimpath --mod=vendor --gcflags "--trimpath $PWD" -asmflags "--trimpath $PWD" --buildmode=plugin -o ./backend.so

FROM heroiclabs/nakama:${NAKAMA_VERSION}

COPY --from=builder /backend/backend.so /nakama/data/modules
COPY --from=builder /backend/local.yml /nakama/data/
