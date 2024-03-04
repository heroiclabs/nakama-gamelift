FROM --platform=linux/amd64 heroiclabs/nakama-pluginbuilder:3.20.1 AS builder

ENV GO111MODULE on
ENV CGO_ENABLED 1

WORKDIR /backend
COPY . .

RUN go build --trimpath --mod=vendor --gcflags "--trimpath $PWD" -asmflags "--trimpath $PWD" --buildmode=plugin -o ./backend.so

FROM --platform=linux/amd64 nakama:dev

COPY --from=builder /backend/backend.so /nakama/data/modules
COPY --from=builder /backend/local.yml /nakama/data/
