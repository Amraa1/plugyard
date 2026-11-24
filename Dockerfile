# syntax=docker/dockerfile:1
#
# The plugyard image: amraa1/plugyard on Docker Hub. The release workflow
# builds and publishes it; "docker build -t plugyard ." builds it here.
#
#   docker run -e CSMS_URL=ws://host.docker.internal:9000/ocpp amraa1/plugyard
#
# starts three chargers. Mount your own site file at /site to run yours, and
# keep /data in a volume so that the chargers keep their meters.

# The program is built on the machine that builds the image, for the system
# the image is for, so that no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
        -ldflags "-s -w -X github.com/Amraa1/plugyard/internal/buildinfo.version=$VERSION" \
        -o /out/plugyard . \
    && mkdir /out/data

# Nothing but the program, certificates and time zones: no shell and no
# package manager. It runs as a user that is not root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/plugyard /usr/local/bin/plugyard
# The data directory belongs to the user plugyard runs as, and a new volume
# mounted there takes that over.
COPY --from=build --chown=nonroot:nonroot /out/data /data
# The site that runs when none is mounted.
COPY docker/site/ /site/
COPY internal/cli/starter/Chargerfile /site/Chargerfile

# PLUGYARD_CONTAINER tells plugyard that it is replaced by pulling a newer
# image, not by "plugyard update".
ENV PLUGYARD_DATA_DIR=/data \
    PLUGYARD_CONTAINER=1
VOLUME /data
WORKDIR /site
EXPOSE 7070

ENTRYPOINT ["plugyard"]
CMD ["site", "up"]
