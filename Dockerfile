FROM heroiclabs/nakama-pluginbuilder:3.21.1 AS builder
# FROM heroiclabs/nakama-pluginbuilder:3.21.1-arm AS builder

ENV GO111MODULE on
ENV CGO_ENABLED 1

WORKDIR /backend
COPY . .

RUN go build --trimpath --mod=vendor --gcflags "--trimpath $PWD" -asmflags "--trimpath $PWD" --buildmode=plugin -o ./backend.so

FROM heroiclabs/nakama:3.21.1
# FROM heroiclabs/nakama:3.21.1-arm

COPY --from=builder /backend/backend.so /nakama/data/modules
COPY --from=builder /backend/local.yml /nakama/data/
