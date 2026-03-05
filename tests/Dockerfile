FROM node:20-slim

# curl is needed by the entrypoint health-check wait loop
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install dependencies first (layer cache friendly)
COPY package.json ./
RUN npm install --omit=dev

# Download ApiDemos APK
ARG APIDEMOS_VERSION=v6.0.6
RUN curl -fsSL \
    "https://github.com/appium/android-apidemos/releases/download/${APIDEMOS_VERSION}/ApiDemos-debug.apk" \
    -o /app/ApiDemos-debug.apk

# Copy test sources
COPY wdio.conf.js ./
COPY test/ ./test/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Runtime env defaults (overridden by docker run -e)
ENV APPIUM_HOST=localhost \
    APPIUM_PORT=4723 \
    ANDROID_SERIAL="" \
    APPIUM_WAIT_SECONDS=60

ENTRYPOINT ["/entrypoint.sh"]
