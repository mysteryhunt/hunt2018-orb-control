FROM golang:1.9.2-alpine3.6

RUN mkdir -p /go/src/github.com/mysteryhunt

ENV GOPATH=/go
COPY . /go/src/github.com/mysteryhunt/hunt2018-orb-control

RUN CGO_ENABLED=0 go install github.com/mysteryhunt/hunt2018-orb-control

FROM scratch
COPY --from=0 /go/bin/hunt2018-orb-control /hunt2018-orb-control

EXPOSE 8080
CMD ["/hunt2018-orb-control"]
