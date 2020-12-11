.PHONY: all
all: build codesign

.PHONY: codesign
codesign:
	codesign --entitlements vz.entitlements -s - ./m1-docker

.PHONY: build
build:
	go build -o m1-docker .
