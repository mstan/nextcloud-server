FROM php:8.3-cli
ADD --chmod=0755 https://github.com/mlocati/docker-php-extension-installer/releases/download/2.7.35/install-php-extensions /usr/local/bin/
RUN install-php-extensions gd intl zip apcu bcmath gmp exif pcntl sysvsem opcache && \
    echo 'apc.enable_cli=1' > /usr/local/etc/php/conf.d/apcu-cli.ini && \
    curl -sSLo /usr/local/bin/composer https://getcomposer.org/download/2.8.12/composer.phar && chmod +x /usr/local/bin/composer
WORKDIR /src
