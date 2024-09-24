FROM heroiclabs/nakama-pluginbuilder:3.23.0 AS builder

ENV GO111MODULE on
ENV CGO_ENABLED 1

WORKDIR /backend
COPY . .

RUN go build --trimpath --mod=vendor --gcflags "--trimpath $PWD" -asmflags "--trimpath $PWD" --buildmode=plugin -o ./backend.so

FROM heroiclabs/nakama:3.23.0

COPY --from=builder /backend/backend.so /nakama/data/modules
COPY --from=builder /backend/local.yml /nakama/data/
