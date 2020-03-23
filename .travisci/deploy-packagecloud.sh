#!/bin/bash

# we do not release deb packages at this point
exit 0

set -xe

gem install package_cloud

packagecloud_url="https://packages.shopify.io"
repo="shopify/ghostferry"

for dist in {ubuntu/trusty,ubuntu/xenial}; do
  package_cloud push --url "$packagecloud_url" "$repo/$dist" build/ghostferry-*.deb
done
