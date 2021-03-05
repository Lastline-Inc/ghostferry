#!/bin/bash

extra_args=""
if [[ "${DEBUG}" == "1" ]] || [[ "${DEBUG}" == "true" ]]; then
    extra_args="${extra_args} --verbose"
fi

eval exec /usr/bin/ghostferry-copydb ${extra_args} /etc/lastline/replication-config.json
