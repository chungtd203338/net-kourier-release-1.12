#!/bin/bash

RED="\e[31m"
GREEN="\e[32m"
BLUE="\e[34m"
YELLOW="\e[33m"
NC="\e[0m"

REDBGR='\033[0;41m'
NCBGR='\033[0m'

########## CONFIG ##########
OPTION=$1
component="kourier"
############################

logSuccess() { echo -e "$GREEN-----$message-----$NC";}
logError() { echo -e "$RED-----$message-----$NC";}
logInfo() { echo -e "$YELLOW###############---$message---###############$NC";}

koBuild() {
    message="ko build image" && logInfo
    export KO_DOCKER_REPO="ko.local"
    ko build ./cmd/$component/
    if [ "$?" -ne "0" ]; then
        message="ko build error" && logError
        exit 1
    else
        message="ko build successfully" && logSuccess
    fi
}

convertImage() {
    message="change image from docker to crictl" && logInfo
    image=$(docker images | grep ko.local | grep $component | grep latest | awk '{print $1}'):latest
    docker rmi -f chung123abc/net-1.12-new-$component1:latest
    docker image tag $image docker.io/chung123abc/net-1.12-new-$component:latest
    docker rmi $image
    image=$(docker images | grep ko.local | grep $component | awk '{print $1}'):$(docker images | grep ko.local | grep $component | awk '{print $2}')
    docker rmi $image
    docker save -o net-1.12-new-$component.tar docker.io/chung123abc/net-1.12-new-$component:latest
    message="Saved atarashi-imeji to .tar file" && logSuccess

    sudo crictl rmi docker.io/chung123abc/net-1.12-new-$component:latest
    sudo ctr -n=k8s.io images import net-1.12-new-$component.tar
    message="Untar atarashi-imeji" && logSuccess
    rm -rf net-1.12-new-$component.tar
}

deployNewVersion() {
    message="remove current Pod" && logInfo
    pod=$(kubectl -n knative-serving get pod | grep "net-1.12-new-$component" | awk '{print $1}')
    kubectl -n knative-serving delete pod/$pod &
}

logPod() {
    sleep 2
    pod=$(kubectl -n knative-serving get pod | grep net-1.12-new-$component | grep -v Terminating | awk '{print $1}')
    kubectl -n knative-serving wait --for=condition=ready pod $pod > /dev/null 2>&1
    curlStatus=$(kubectl exec curl-test -- curl -I hello.default.svc.cluster.local | head -n 1| cut -d $' ' -f2)
    echo $curlStatus
    clear
    if [ $curlStatus -eq "200" ]; then
        message="curl success" && logSuccess
    else
        message="curl failed" && logError
    fi
    message="net-$component logs" && logInfo
    kubectl -n knative-serving logs $pod -f
}

clear


echo -e "ok"
# echo -e "$REDBGR このスクリプトはボナちゃんによって書かれています $NCBGR"



if [ $OPTION == "ko" ]; then
    image=$(docker images | grep ko.local | grep $component | awk '{print $3}')
    docker rmi -f $image
    koBuild
elif [ $OPTION == "dep" ]; then
    # convertImage
    deployNewVersion
    # logPod
elif [ $OPTION == "log" ]; then
    deployNewVersion
    logPod
elif [ $OPTION == "ful" ]; then
    koBuild
    if [ $? -eq "0" ]; then
        convertImage
        # deployNewVersion
        # logPod
    else
        exit 1
    fi
elif [ $OPTION == "clean" ]; then
    go clean -cache
    # ./hack/update-deps.sh --upgrade
    # ./hack/update-codegen.sh
    clear
    koBuild
    convertImage
    deployNewVersion
    logPod
fi
