set -e
appName="openlist"
builtAt="$(date +'%F %T %z')"
gitAuthor="The OpenList Projects Contributors <noreply@openlist.team>"
gitCommit=$(git log --pretty=format:"%h" -1)

githubAuthHeader=""
githubAuthValue=""
if [ -n "$GITHUB_TOKEN" ]; then
  githubAuthHeader="--header"
  githubAuthValue="Authorization: Bearer $GITHUB_TOKEN"
fi

if [ "$1" = "dev" ]; then
  version="dev"
  webVersion="dev"
elif [ "$1" = "beta" ]; then
  version="beta"
  webVersion="dev"
else
  git tag -d beta || true
  # Always true if there's no tag
  version=$(git describe --abbrev=0 --tags 2>/dev/null || echo "v0.0.0")
  webVersion=$(curl -fsSL --max-time 2 $githubAuthHeader $githubAuthValue "https://api.github.com/repos/OpenListTeam/OpenList-Frontend/releases/latest" | grep "tag_name" | head -n 1 | awk -F ":" '{print $2}' | sed 's/\"//g;s/,//g;s/ //g')
fi

echo "backend version: $version"
echo "frontend version: $webVersion"

ldflags="\
-w -s \
-X 'github.com/OpenListTeam/OpenList/internal/conf.BuiltAt=$builtAt' \
-X 'github.com/OpenListTeam/OpenList/internal/conf.GitAuthor=$gitAuthor' \
-X 'github.com/OpenListTeam/OpenList/internal/conf.GitCommit=$gitCommit' \
-X 'github.com/OpenListTeam/OpenList/internal/conf.Version=$version' \
-X 'github.com/OpenListTeam/OpenList/internal/conf.WebVersion=$webVersion' \
"

FetchWebDev() {
  pre_release_tag=$(curl -fsSL --max-time 2 $githubAuthHeader $githubAuthValue https://api.github.com/repos/OpenListTeam/OpenList-Frontend/releases | jq -r 'map(select(.prerelease)) | first | .tag_name')
  if [ -z "$pre_release_tag" ] || [ "$pre_release_tag" == "null" ]; then
    # fall back to latest release
    pre_release_json=$(curl -fsSL --max-time 2 $githubAuthHeader $githubAuthValue -H "Accept: application/vnd.github.v3+json" "https://api.github.com/repos/OpenListTeam/OpenList-Frontend/releases/latest")
  else
    pre_release_json=$(curl -fsSL --max-time 2 $githubAuthHeader $githubAuthValue -H "Accept: application/vnd.github.v3+json" "https://api.github.com/repos/OpenListTeam/OpenList-Frontend/releases/tags/$pre_release_tag")
  fi
  pre_release_assets=$(echo "$pre_release_json" | jq -r '.assets[].browser_download_url')
  pre_release_tar_url=$(echo "$pre_release_assets" | grep "openlist-frontend-dist" | grep "\.tar\.gz$")
  curl -fsSL "$pre_release_tar_url" -o web-dist-dev.tar.gz
  rm -rf public/dist && mkdir -p public/dist
  tar -zxvf web-dist-dev.tar.gz -C public/dist
  rm -rf web-dist-dev.tar.gz
}

FetchWebRelease() {
  release_json=$(curl -fsSL --max-time 2 $githubAuthHeader $githubAuthValue -H "Accept: application/vnd.github.v3+json" "https://api.github.com/repos/OpenListTeam/OpenList-Frontend/releases/latest")
  release_assets=$(echo "$release_json" | jq -r '.assets[].browser_download_url')
  release_tar_url=$(echo "$release_assets" | grep "openlist-frontend-dist" | grep "\.tar\.gz$")
  curl -fsSL "$release_tar_url" -o dist.tar.gz
  rm -rf public/dist && mkdir -p public/dist
  tar -zxvf dist.tar.gz -C public/dist
  rm -rf dist.tar.gz
}

# Create CDN version with only index.html
CreateCdnVersion() {
  echo "Creating CDN version..."
  rm -rf public/dist-cdn && mkdir -p public/dist-cdn
  # Only copy index.html and essential files for CDN version
  cp public/dist/index.html public/dist-cdn/
  if [ -f public/dist/README.md ]; then
    cp public/dist/README.md public/dist-cdn/
  fi
  if [ -f public/dist/VERSION ]; then
    cp public/dist/VERSION public/dist-cdn/
  fi
  echo "CDN version created with minimal files"
}

BuildWinArm64() {
  echo building for windows-arm64
  chmod +x ./wrapper/zcc-arm64
  chmod +x ./wrapper/zcxx-arm64
  export GOOS=windows
  export GOARCH=arm64
  export CC=$(pwd)/wrapper/zcc-arm64
  export CXX=$(pwd)/wrapper/zcxx-arm64
  export CGO_ENABLED=1
  if [[ "$1" == *"-cdn"* ]]; then
    go build -o "$1" -ldflags="$ldflags" -tags="jsoniter cdn" .
  else
    go build -o "$1" -ldflags="$ldflags" -tags=jsoniter .
  fi
}

BuildDev() {
  rm -rf .git/
  mkdir -p "dist"
  muslflags="--extldflags '-static -fpic' $ldflags"
  BASE="https://github.com/OpenListTeam/musl-compilers/releases/latest/download/"
  FILES=(x86_64-linux-musl-cross aarch64-linux-musl-cross)
  for i in "${FILES[@]}"; do
    url="${BASE}${i}.tgz"
    curl -fsSL -o "${i}.tgz" "${url}"
    sudo tar xf "${i}.tgz" --strip-components 1 -C /usr/local
  done
  OS_ARCHES=(linux-musl-amd64 linux-musl-arm64)
  CGO_ARGS=(x86_64-linux-musl-gcc aarch64-linux-musl-gcc)
  
  # Check if only CDN version should be built
  if [ "$3" = "cdn-only" ]; then
    echo "Building CDN version only..."
    CreateCdnVersion
    for i in "${!OS_ARCHES[@]}"; do
      os_arch=${OS_ARCHES[$i]}
      cgo_cc=${CGO_ARGS[$i]}
      echo building CDN version for ${os_arch}
      export GOOS=${os_arch%%-*}
      export GOARCH=${os_arch##*-}
      export CC=${cgo_cc}
      export CGO_ENABLED=1
      go build -o ./dist/$appName-$os_arch-cdn -ldflags="$muslflags" -tags="jsoniter cdn" .
    done
    xgo -targets=windows/amd64,darwin/amd64,darwin/arm64 -out "$appName-cdn" -ldflags="$ldflags" -tags="jsoniter cdn" .
    mv "$appName-cdn"-* dist
    # Rename CDN binaries to have -cdn suffix
    cd dist
    for file in "$appName-cdn"-*; do
      if [[ "$file" != *"-cdn-"* ]]; then
        newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.$/$-cdn./")
        if [[ "$file" == *".exe" ]]; then
          newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.exe$/-cdn.exe/")
        else
          newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/$/-cdn/")
        fi
        mv "$file" "$newname"
      fi
    done
    cd ..
    
    cd dist
    if [ -f ./"$appName"-windows-amd64-cdn.exe ]; then
      cp ./"$appName"-windows-amd64-cdn.exe ./"$appName"-windows-amd64-cdn-upx.exe
      upx -9 ./"$appName"-windows-amd64-cdn-upx.exe
    fi
    find . -type f -print0 | xargs -0 md5sum >md5.txt
    cat md5.txt
    return
  fi
  
  # Build standard version
  echo "Building standard version..."
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    echo building for ${os_arch}
    export GOOS=${os_arch%%-*}
    export GOARCH=${os_arch##*-}
    export CC=${cgo_cc}
    export CGO_ENABLED=1
    go build -o ./dist/$appName-$os_arch -ldflags="$muslflags" -tags=jsoniter .
  done
  xgo -targets=windows/amd64,darwin/amd64,darwin/arm64 -out "$appName" -ldflags="$ldflags" -tags=jsoniter .
  mv "$appName"-* dist
  
  # Build CDN version
  echo "Building CDN version..."
  CreateCdnVersion
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    echo building CDN version for ${os_arch}
    export GOOS=${os_arch%%-*}
    export GOARCH=${os_arch##*-}
    export CC=${cgo_cc}
    export CGO_ENABLED=1
    go build -o ./dist/$appName-$os_arch-cdn -ldflags="$muslflags" -tags="jsoniter cdn" .
  done
  xgo -targets=windows/amd64,darwin/amd64,darwin/arm64 -out "$appName-cdn" -ldflags="$ldflags" -tags="jsoniter cdn" .
  mv "$appName-cdn"-* dist
  # Rename CDN binaries to have -cdn suffix
  cd dist
  for file in "$appName-cdn"-*; do
    if [[ "$file" != *"-cdn-"* ]]; then
      newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.$/$-cdn./")
      if [[ "$file" == *".exe" ]]; then
        newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.exe$/-cdn.exe/")
      else
        newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/$/-cdn/")
      fi
      mv "$file" "$newname"
    fi
  done
  cd ..
  
  cd dist
  cp ./"$appName"-windows-amd64.exe ./"$appName"-windows-amd64-upx.exe
  upx -9 ./"$appName"-windows-amd64-upx.exe
  if [ -f ./"$appName"-windows-amd64-cdn.exe ]; then
    cp ./"$appName"-windows-amd64-cdn.exe ./"$appName"-windows-amd64-cdn-upx.exe
    upx -9 ./"$appName"-windows-amd64-cdn-upx.exe
  fi
  find . -type f -print0 | xargs -0 md5sum >md5.txt
  cat md5.txt
}

BuildDocker() {
  # Check if only CDN version should be built
  if [ "$3" = "cdn-only" ]; then
    echo "Building CDN version only for Docker..."
    go build -o ./bin/"$appName"-cdn -ldflags="$ldflags" -tags="jsoniter cdn" .
    return
  fi
  
  # Build standard version
  go build -o ./bin/"$appName" -ldflags="$ldflags" -tags=jsoniter .
  # Build CDN version
  go build -o ./bin/"$appName"-cdn -ldflags="$ldflags" -tags="jsoniter cdn" .
}

PrepareBuildDockerMusl() {
  mkdir -p build/musl-libs
  BASE="https://github.com/OpenListTeam/musl-compilers/releases/latest/download/"
  FILES=(x86_64-linux-musl-cross aarch64-linux-musl-cross i486-linux-musl-cross s390x-linux-musl-cross armv6-linux-musleabihf-cross armv7l-linux-musleabihf-cross riscv64-linux-musl-cross powerpc64le-linux-musl-cross)
  for i in "${FILES[@]}"; do
    url="${BASE}${i}.tgz"
    lib_tgz="build/${i}.tgz"
    curl -fsSL -o "${lib_tgz}" "${url}"
    tar xf "${lib_tgz}" --strip-components 1 -C build/musl-libs
    rm -f "${lib_tgz}"
  done
}

BuildDockerMultiplatform() {
  go mod download

  # run PrepareBuildDockerMusl before build
  export PATH=$PATH:$PWD/build/musl-libs/bin

  docker_lflags="--extldflags '-static -fpic' $ldflags"
  export CGO_ENABLED=1

  OS_ARCHES=(linux-amd64 linux-arm64 linux-386 linux-s390x linux-riscv64 linux-ppc64le)
  CGO_ARGS=(x86_64-linux-musl-gcc aarch64-linux-musl-gcc i486-linux-musl-gcc s390x-linux-musl-gcc riscv64-linux-musl-gcc powerpc64le-linux-musl-gcc)
  
  # Check if only CDN version should be built
  if [ "$3" = "cdn-only" ]; then
    echo "Building CDN version only for Docker multiplatform..."
    # Build CDN version
    for i in "${!OS_ARCHES[@]}"; do
      os_arch=${OS_ARCHES[$i]}
      cgo_cc=${CGO_ARGS[$i]}
      os=${os_arch%%-*}
      arch=${os_arch##*-}
      export GOOS=$os
      export GOARCH=$arch
      export CC=${cgo_cc}
      echo "building CDN version for $os_arch"
      go build -o build/$os/$arch/"$appName"-cdn -ldflags="$docker_lflags" -tags="jsoniter cdn" .
    done

    DOCKER_ARM_ARCHES=(linux-arm/v6 linux-arm/v7)
    CGO_ARGS=(armv6-linux-musleabihf-gcc armv7l-linux-musleabihf-gcc)
    GO_ARM=(6 7)
    export GOOS=linux
    export GOARCH=arm
    
    # Build CDN version for ARM
    for i in "${!DOCKER_ARM_ARCHES[@]}"; do
      docker_arch=${DOCKER_ARM_ARCHES[$i]}
      cgo_cc=${CGO_ARGS[$i]}
      export GOARM=${GO_ARM[$i]}
      export CC=${cgo_cc}
      echo "building CDN version for $docker_arch"
      go build -o build/${docker_arch%%-*}/${docker_arch##*-}/"$appName"-cdn -ldflags="$docker_lflags" -tags="jsoniter cdn" .
    done
    return
  fi
  
  # Build standard version
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    os=${os_arch%%-*}
    arch=${os_arch##*-}
    export GOOS=$os
    export GOARCH=$arch
    export CC=${cgo_cc}
    echo "building standard version for $os_arch"
    go build -o build/$os/$arch/"$appName" -ldflags="$docker_lflags" -tags=jsoniter .
  done

  # Build CDN version
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    os=${os_arch%%-*}
    arch=${os_arch##*-}
    export GOOS=$os
    export GOARCH=$arch
    export CC=${cgo_cc}
    echo "building CDN version for $os_arch"
    go build -o build/$os/$arch/"$appName"-cdn -ldflags="$docker_lflags" -tags="jsoniter cdn" .
  done

  DOCKER_ARM_ARCHES=(linux-arm/v6 linux-arm/v7)
  CGO_ARGS=(armv6-linux-musleabihf-gcc armv7l-linux-musleabihf-gcc)
  GO_ARM=(6 7)
  export GOOS=linux
  export GOARCH=arm
  
  # Build standard version for ARM
  for i in "${!DOCKER_ARM_ARCHES[@]}"; do
    docker_arch=${DOCKER_ARM_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    export GOARM=${GO_ARM[$i]}
    export CC=${cgo_cc}
    echo "building standard version for $docker_arch"
    go build -o build/${docker_arch%%-*}/${docker_arch##*-}/"$appName" -ldflags="$docker_lflags" -tags=jsoniter .
  done
  
  # Build CDN version for ARM
  for i in "${!DOCKER_ARM_ARCHES[@]}"; do
    docker_arch=${DOCKER_ARM_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    export GOARM=${GO_ARM[$i]}
    export CC=${cgo_cc}
    echo "building CDN version for $docker_arch"
    go build -o build/${docker_arch%%-*}/${docker_arch##*-}/"$appName"-cdn -ldflags="$docker_lflags" -tags="jsoniter cdn" .
  done
}

BuildRelease() {
  rm -rf .git/
  mkdir -p "build"
  
  # Check if only CDN version should be built
  if [ "$3" = "cdn-only" ]; then
    echo "Building CDN release version only..."
    CreateCdnVersion
    BuildWinArm64 ./build/"$appName"-windows-arm64-cdn.exe
    xgo -out "$appName-cdn" -ldflags="$ldflags" -tags="jsoniter cdn" .
    upx -9 ./"$appName-cdn"-linux-amd64
    cp ./"$appName-cdn"-windows-amd64.exe ./"$appName-cdn"-windows-amd64-upx.exe
    upx -9 ./"$appName-cdn"-windows-amd64-upx.exe
    
    # Rename CDN binaries to have -cdn suffix
    for file in "$appName-cdn"-*; do
      if [[ "$file" != *"-cdn-"* ]]; then
        newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.$/$-cdn./")
        if [[ "$file" == *".exe" ]]; then
          newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.exe$/-cdn.exe/")
        else
          newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/$/-cdn/")
        fi
        mv "$file" "build/$newname"
      else
        mv "$file" build/
      fi
    done
    return
  fi
  
  # Build standard version
  echo "Building standard release version..."
  BuildWinArm64 ./build/"$appName"-windows-arm64.exe
  xgo -out "$appName" -ldflags="$ldflags" -tags=jsoniter .
  # why? Because some target platforms seem to have issues with upx compression
  upx -9 ./"$appName"-linux-amd64
  cp ./"$appName"-windows-amd64.exe ./"$appName"-windows-amd64-upx.exe
  upx -9 ./"$appName"-windows-amd64-upx.exe
  mv "$appName"-* build
  
  # Build CDN version
  echo "Building CDN release version..."
  CreateCdnVersion
  BuildWinArm64 ./build/"$appName"-windows-arm64-cdn.exe
  xgo -out "$appName-cdn" -ldflags="$ldflags" -tags="jsoniter cdn" .
  upx -9 ./"$appName-cdn"-linux-amd64
  cp ./"$appName-cdn"-windows-amd64.exe ./"$appName-cdn"-windows-amd64-upx.exe
  upx -9 ./"$appName-cdn"-windows-amd64-upx.exe
  
  # Rename CDN binaries to have -cdn suffix
  for file in "$appName-cdn"-*; do
    if [[ "$file" != *"-cdn-"* ]]; then
      newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.$/$-cdn./")
      if [[ "$file" == *".exe" ]]; then
        newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/\.exe$/-cdn.exe/")
      else
        newname=$(echo "$file" | sed "s/$appName-cdn-/$appName-/" | sed "s/$/-cdn/")
      fi
      mv "$file" "build/$newname"
    else
      mv "$file" build/
    fi
  done
}

BuildReleaseLinuxMusl() {
  rm -rf .git/
  mkdir -p "build"
  muslflags="--extldflags '-static -fpic' $ldflags"
  BASE="https://github.com/OpenListTeam/musl-compilers/releases/latest/download/"
  FILES=(x86_64-linux-musl-cross aarch64-linux-musl-cross mips-linux-musl-cross mips64-linux-musl-cross mips64el-linux-musl-cross mipsel-linux-musl-cross powerpc64le-linux-musl-cross s390x-linux-musl-cross loongarch64-linux-musl-cross)
  for i in "${FILES[@]}"; do
    url="${BASE}${i}.tgz"
    curl -fsSL -o "${i}.tgz" "${url}"
    sudo tar xf "${i}.tgz" --strip-components 1 -C /usr/local
    rm -f "${i}.tgz"
  done
  OS_ARCHES=(linux-musl-amd64 linux-musl-arm64 linux-musl-mips linux-musl-mips64 linux-musl-mips64le linux-musl-mipsle linux-musl-ppc64le linux-musl-s390x linux-musl-loong64)
  CGO_ARGS=(x86_64-linux-musl-gcc aarch64-linux-musl-gcc mips-linux-musl-gcc mips64-linux-musl-gcc mips64el-linux-musl-gcc mipsel-linux-musl-gcc powerpc64le-linux-musl-gcc s390x-linux-musl-gcc loongarch64-linux-musl-gcc)
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    echo building for ${os_arch}
    export GOOS=${os_arch%%-*}
    export GOARCH=${os_arch##*-}
    export CC=${cgo_cc}
    export CGO_ENABLED=1
    go build -o ./build/$appName-$os_arch -ldflags="$muslflags" -tags=jsoniter .
  done
}

BuildReleaseLinuxMuslArm() {
  rm -rf .git/
  mkdir -p "build"
  muslflags="--extldflags '-static -fpic' $ldflags"
  BASE="https://github.com/OpenListTeam/musl-compilers/releases/latest/download/"
  FILES=(arm-linux-musleabi-cross arm-linux-musleabihf-cross armel-linux-musleabi-cross armel-linux-musleabihf-cross armv5l-linux-musleabi-cross armv5l-linux-musleabihf-cross armv6-linux-musleabi-cross armv6-linux-musleabihf-cross armv7l-linux-musleabihf-cross armv7m-linux-musleabi-cross armv7r-linux-musleabihf-cross)
  for i in "${FILES[@]}"; do
    url="${BASE}${i}.tgz"
    curl -fsSL -o "${i}.tgz" "${url}"
    sudo tar xf "${i}.tgz" --strip-components 1 -C /usr/local
    rm -f "${i}.tgz"
  done
  OS_ARCHES=(linux-musleabi-arm linux-musleabihf-arm linux-musleabi-armel linux-musleabihf-armel linux-musleabi-armv5l linux-musleabihf-armv5l linux-musleabi-armv6 linux-musleabihf-armv6 linux-musleabihf-armv7l linux-musleabi-armv7m linux-musleabihf-armv7r)
  CGO_ARGS=(arm-linux-musleabi-gcc arm-linux-musleabihf-gcc armel-linux-musleabi-gcc armel-linux-musleabihf-gcc armv5l-linux-musleabi-gcc armv5l-linux-musleabihf-gcc armv6-linux-musleabi-gcc armv6-linux-musleabihf-gcc armv7l-linux-musleabihf-gcc armv7m-linux-musleabi-gcc armv7r-linux-musleabihf-gcc)
  GOARMS=('' '' '' '' '5' '5' '6' '6' '7' '7' '7')
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=${CGO_ARGS[$i]}
    arm=${GOARMS[$i]}
    echo building for ${os_arch}
    export GOOS=linux
    export GOARCH=arm
    export CC=${cgo_cc}
    export CGO_ENABLED=1
    export GOARM=${arm}
    go build -o ./build/$appName-$os_arch -ldflags="$muslflags" -tags=jsoniter .
  done
}

BuildReleaseAndroid() {
  rm -rf .git/
  mkdir -p "build"
  wget https://dl.google.com/android/repository/android-ndk-r26b-linux.zip
  unzip android-ndk-r26b-linux.zip
  rm android-ndk-r26b-linux.zip
  OS_ARCHES=(amd64 arm64 386 arm)
  CGO_ARGS=(x86_64-linux-android24-clang aarch64-linux-android24-clang i686-linux-android24-clang armv7a-linux-androideabi24-clang)
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc=$(realpath android-ndk-r26b/toolchains/llvm/prebuilt/linux-x86_64/bin/${CGO_ARGS[$i]})
    echo building for android-${os_arch}
    export GOOS=android
    export GOARCH=${os_arch##*-}
    export CC=${cgo_cc}
    export CGO_ENABLED=1
    go build -o ./build/$appName-android-$os_arch -ldflags="$ldflags" -tags=jsoniter .
    android-ndk-r26b/toolchains/llvm/prebuilt/linux-x86_64/bin/llvm-strip ./build/$appName-android-$os_arch
  done
}

BuildReleaseFreeBSD() {
  rm -rf .git/
  mkdir -p "build/freebsd"
  
  # Get latest FreeBSD 14.x release version from GitHub 
  freebsd_version=$(curl -fsSL --max-time 2 $githubAuthHeader $githubAuthValue "https://api.github.com/repos/freebsd/freebsd-src/tags" | \
    jq -r '.[].name' | \
    grep '^release/14\.' | \
    sort -V | \
    tail -1 | \
    sed 's/release\///' | \
    sed 's/\.0$//')
  
  if [ -z "$freebsd_version" ]; then
    echo "Failed to get FreeBSD version, falling back to 14.3"
    freebsd_version="14.3"
  fi

  echo "Using FreeBSD version: $freebsd_version"
  
  OS_ARCHES=(amd64 arm64 i386)
  GO_ARCHES=(amd64 arm64 386)
  CGO_ARGS=(x86_64-unknown-freebsd${freebsd_version} aarch64-unknown-freebsd${freebsd_version} i386-unknown-freebsd${freebsd_version})
  for i in "${!OS_ARCHES[@]}"; do
    os_arch=${OS_ARCHES[$i]}
    cgo_cc="clang --target=${CGO_ARGS[$i]} --sysroot=/opt/freebsd/${os_arch}"
    echo building for freebsd-${os_arch}
    sudo mkdir -p "/opt/freebsd/${os_arch}"
    wget -q https://download.freebsd.org/releases/${os_arch}/${freebsd_version}-RELEASE/base.txz
    sudo tar -xf ./base.txz -C /opt/freebsd/${os_arch}
    rm base.txz
    export GOOS=freebsd
    export GOARCH=${GO_ARCHES[$i]}
    export CC=${cgo_cc}
    export CGO_ENABLED=1
    export CGO_LDFLAGS="-fuse-ld=lld"
    go build -o ./build/$appName-freebsd-$os_arch -ldflags="$ldflags" -tags=jsoniter .
  done
}

MakeRelease() {
  cd build
  if [ -d compress ]; then
    rm -rv compress
  fi
  mkdir compress
  for i in $(find . -type f -name "$appName-linux-*"); do
    cp "$i" "$appName"
    tar -czvf compress/"$i".tar.gz "$appName"
    rm -f "$appName"
  done
    for i in $(find . -type f -name "$appName-android-*"); do
    cp "$i" "$appName"
    tar -czvf compress/"$i".tar.gz "$appName"
    rm -f "$appName"
  done
  for i in $(find . -type f -name "$appName-darwin-*"); do
    cp "$i" "$appName"
    tar -czvf compress/"$i".tar.gz "$appName"
    rm -f "$appName"
  done
  for i in $(find . -type f -name "$appName-freebsd-*"); do
    cp "$i" "$appName"
    tar -czvf compress/"$i".tar.gz "$appName"
    rm -f "$appName"
  done
  for i in $(find . -type f -name "$appName-windows-*"); do
    cp "$i" "$appName".exe
    zip compress/$(echo $i | sed 's/\.[^.]*$//').zip "$appName".exe
    rm -f "$appName".exe
  done
  cd compress
  find . -type f -print0 | xargs -0 md5sum >"$1"
  cat "$1"
  cd ../..
}

if [ "$1" = "dev" ]; then
  FetchWebDev
  # Only create CDN version if not building standard version only
  if [ "$3" != "standard-only" ]; then
    CreateCdnVersion
  fi
  if [ "$2" = "docker" ]; then
    BuildDocker "$1" "$2" "$3"
  elif [ "$2" = "docker-multiplatform" ]; then
      BuildDockerMultiplatform "$1" "$2" "$3"
  elif [ "$2" = "web" ]; then
    echo "web only"
  else
    BuildDev "$1" "$2" "$3"
  fi
elif [ "$1" = "release" -o "$1" = "beta" ]; then
  if [ "$1" = "beta" ]; then
    FetchWebDev
  else
    FetchWebRelease
  fi
  # Only create CDN version if not building standard version only
  if [ "$3" != "standard-only" ]; then
    CreateCdnVersion
  fi
  if [ "$2" = "docker" ]; then
    BuildDocker "$1" "$2" "$3"
  elif [ "$2" = "docker-multiplatform" ]; then
    BuildDockerMultiplatform "$1" "$2" "$3"
  elif [ "$2" = "linux_musl_arm" ]; then
    BuildReleaseLinuxMuslArm "$1" "$2" "$3"
    MakeRelease "md5-linux-musl-arm.txt"
  elif [ "$2" = "linux_musl" ]; then
    BuildReleaseLinuxMusl "$1" "$2" "$3"
    MakeRelease "md5-linux-musl.txt"
  elif [ "$2" = "android" ]; then
    BuildReleaseAndroid "$1" "$2" "$3"
    MakeRelease "md5-android.txt"
  elif [ "$2" = "freebsd" ]; then
    BuildReleaseFreeBSD "$1" "$2" "$3"
    MakeRelease "md5-freebsd.txt"
  elif [ "$2" = "web" ]; then
    echo "web only"
  else
    BuildRelease "$1" "$2" "$3"
    MakeRelease "md5.txt"
  fi
elif [ "$1" = "prepare" ]; then
  if [ "$2" = "docker-multiplatform" ]; then
    PrepareBuildDockerMusl
  fi
elif [ "$1" = "zip" ]; then
  MakeRelease "$2".txt
else
  echo -e "Parameter error"
  echo "Usage:"
  echo "  $0 <version> [build-type] [build-mode]"
  echo ""
  echo "Version:"
  echo "  dev      - Development build"
  echo "  beta     - Beta build"
  echo "  release  - Release build"
  echo ""
  echo "Build Type:"
  echo "  docker                - Docker build"
  echo "  docker-multiplatform  - Docker multiplatform build"
  echo "  linux_musl           - Linux musl build"
  echo "  linux_musl_arm       - Linux musl ARM build"
  echo "  android              - Android build"
  echo "  freebsd              - FreeBSD build"
  echo "  web                  - Web only (no build)"
  echo ""
  echo "Build Mode:"
  echo "  cdn-only      - Build only CDN version"
  echo "  standard-only - Build only standard version"
  echo "  (default)     - Build both versions"
  echo ""
  echo "Examples:"
  echo "  $0 dev                           # Build both versions for dev"
  echo "  $0 beta docker-multiplatform     # Build both versions for beta docker multiplatform"
  echo "  $0 release docker cdn-only       # Build only CDN version for release docker"
  echo "  $0 dev standard-only             # Build only standard version for dev"
fi
