#!/bin/bash
GIT_TAG=$(git describe --exact-match --tags HEAD 2>/dev/null)
VERSION="unknown"

echo "Got tag:\"${GIT_TAG}\""
if [ -z $GIT_TAG ]; then
  GIT_BRANCH=$(git branch | grep \* | cut -d ' ' -f2)
  echo "Got branch:\"${GIT_BRANCH}\""
  if [ "$GIT_BRANCH" == "master" ]; then
    VERSION="latest"
  fi
  if [ "$TRAVIS_BRANCH" == "master" ] ; then
    VERSION="latest"
  fi
else
  VERSION=$GIT_TAG
fi

set -e

echo "---------------------"
echo "  Building khostdns  "
echo "---------------------"

docker run --rm -e VERSION=${VERSION} -e HOME=/tmp -u $(id -u ${USER}):$(id -g ${USER}) -v "$PWD":/go/khostdns -w /go/khostdns golang:1.12.5 \
./build.sh

echo ""
echo "---------------------"
echo "Building khostdns Container version: ${VERSION}"
echo "---------------------"

DTAG="premiereglobal/khostdns:${VERSION}"

docker build . -t ${DTAG}

echo "---------------------"
echo "Created Tag ${DTAG}"
echo "---------------------"

if [[ ${TRAVIS} && "${VERSION}" != "unknown" && -n $DOCKER_USERNAME && -n $DOCKER_PASSWORD ]]; then
  echo "Pushing docker image: ${DTAG}"
  docker login -u="$DOCKER_USERNAME" -p="$DOCKER_PASSWORD"
  docker push ${DTAG}
#elif [[ ${TRAVIS} && -z $DOCKER_USERNAME ]]; then
#  echo "No Docker user set"
#elif [[ ${TRAVIS} && -z $DOCKER_PASSWORD ]]; then
#  echo "No Docker user set"
fi

