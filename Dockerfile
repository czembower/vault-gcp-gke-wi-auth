FROM alpine:latest

COPY ./vggwa /bin/vggwa
RUN chmod 777 /bin/vggwa

ENTRYPOINT [ "/bin/vggwa" ]