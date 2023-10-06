# Build the svelte kit project
FROM node:20.7.0-alpine as build

WORKDIR /app

COPY package.json package-lock.json ./

RUN npm ci --silent

COPY . ./

# Pass through the necessary environment variables
ARG SITE_URL=${SITE_URL}
ARG DB_URL=${DB_URL}
ARG SENTRY_DSN=${SENTRY_DSN}
ARG PUBLIC_GOOGLE_ANALYTICS_SITE_ID=${PUBLIC_GOOGLE_ANALYTICS_SITE_ID}
ARG PUBLIC_PLAUSIBLE_URL=${PUBLIC_PLAUSIBLE_URL}
ARG PUBLIC_PLAUSIBLE_DOMAIN=${PUBLIC_PLAUSIBLE_DOMAIN}
ARG PUBLIC_ACKEE_URL=${PUBLIC_ACKEE_URL}
ARG PUBLIC_ACKEE_DOMAIN_ID=${PUBLIC_ACKEE_DOMAIN_ID}
ARG PUBLIC_MATOMO_URL=${PUBLIC_MATOMO_URL}
ARG PUBLIC_MATOMO_SITE_ID=${PUBLIC_MATOMO_SITE_ID}

ENV SITE_URL=${SITE_URL}
ENV DB_URL=${DB_URL}
ENV SENTRY_DSN=${SENTRY_DSN}
ENV PUBLIC_GOOGLE_ANALYTICS_SITE_ID=${PUBLIC_GOOGLE_ANALYTICS_SITE_ID}
ENV PUBLIC_PLAUSIBLE_URL=${PUBLIC_PLAUSIBLE_URL}
ENV PUBLIC_PLAUSIBLE_DOMAIN=${PUBLIC_PLAUSIBLE_DOMAIN}
ENV PUBLIC_ACKEE_URL=${PUBLIC_ACKEE_URL}
ENV PUBLIC_ACKEE_DOMAIN_ID=${PUBLIC_ACKEE_DOMAIN_ID}
ENV PUBLIC_MATOMO_URL=${PUBLIC_MATOMO_URL}
ENV PUBLIC_MATOMO_SITE_ID=${PUBLIC_MATOMO_SITE_ID}

RUN npm run build

CMD ["npm", "run", "preview"]
