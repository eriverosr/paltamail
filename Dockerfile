FROM golang:1.15.6
COPY ./ /kiwi/
WORKDIR /kiwi/
EXPOSE 8080
EXPOSE 25
RUN go mod tidy
RUN go build
ENTRYPOINT ["/kiwi/burner.kiwi"]
