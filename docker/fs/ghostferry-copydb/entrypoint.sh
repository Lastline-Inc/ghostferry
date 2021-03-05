#!/bin/bash

set -e

if [[ "${DEBUG}" == "1" ]] || [[ "${DEBUG}" == "true" ]];
then
    tiller --verbose --debug
else
    tiller
fi

if [ "$#" -eq 0 ]; then
    # run the default command for this image
    CONTAINER_COMMAND=/container_command.sh
else
    # run the provided command in the container
    echo 'Running custom container command'
    CONTAINER_COMMAND=$@
fi

exec $CONTAINER_COMMAND
