ARG PHP_VERSION=8.4
FROM php:${PHP_VERSION}-fpm-alpine
RUN apk add --no-cache nginx supervisor git unzip icu-dev libzip-dev oniguruma-dev \
 && docker-php-ext-install pdo_mysql mysqli intl zip mbstring opcache \
 && mkdir -p /home/container
COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer
COPY php.ini /usr/local/etc/php/conf.d/hive-web.ini
COPY hive-web /usr/local/bin/hive-web
RUN chmod 755 /usr/local/bin/hive-web
ENV PORT=8080 DOCUMENT_ROOT=public HOME=/home/container
WORKDIR /home/container
CMD ["/usr/local/bin/hive-web"]
