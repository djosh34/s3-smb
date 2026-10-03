# Mac acceptance fixture

A small command for the MinIO bucket that `test/macos/acceptance.py` uses. It
reads the MinIO credentials from `MINIO_ROOT_USER` and `MINIO_ROOT_PASSWORD` and
accepts only a loopback endpoint.

```sh
go build -o fixture ./test/macos/fixture
fixture bucket-create --endpoint http://127.0.0.1:19000 --bucket time-machine
fixture bucket-list --endpoint http://127.0.0.1:19000 --bucket time-machine --prefix s3-smb/meta/
```

`bucket-list` prints one JSON object with the key and size of every object
under the prefix.
