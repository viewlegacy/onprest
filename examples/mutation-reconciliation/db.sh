#!/usr/bin/env bash
# Disposable, loopback-only databases for this example. No existing DB is modified.
set -euo pipefail
cd "$(dirname "$0")"
if [[ $# != 2 ]]; then
  echo "Usage: $0 up|down postgres|mysql|sqlserver|oracle" >&2
  exit 2
fi
action=$1
db=$2
case "$db" in postgres|mysql|sqlserver|oracle) ;; *) echo "Unknown database: $db" >&2; exit 2 ;; esac
    docker info >/dev/null
name="onprest-recon-$db"
label="com.onprest.example=mutation-reconciliation"
if [[ "$action" == down ]]; then
  if ! docker container inspect "$name" >/dev/null 2>&1; then
    echo "No example container: $name"
    exit 0
  fi
  owner=$(docker inspect --format '{{ index .Config.Labels "com.onprest.example" }}' "$name")
  [[ "$owner" == mutation-reconciliation ]] || { echo "Refusing to remove an unrelated container: $name" >&2; exit 1; }
  docker rm -f -v "$name"
  exit 0
fi
[[ "$action" == up ]] || { echo "Unknown action: $action" >&2; exit 2; }
if docker container inspect "$name" >/dev/null 2>&1; then
  echo "$name already exists; use '$0 down $db' before starting a fresh example." >&2
  exit 1
fi
created=false
cleanup_on_error() {
  status=$?
  if [[ "$status" != 0 && "$created" == true ]]; then
    docker logs --tail 20 "$name" >&2 || true
    docker rm -f -v "$name" >/dev/null || true
  fi
  exit "$status"
}
trap cleanup_on_error EXIT
ready() {
  # Check through TCP so temporary initialization servers do not count as ready.
  case "$db" in
    postgres)
    docker exec -e PGCONNECT_TIMEOUT=3 -e PGPASSWORD=Onprest-admin-1 onprest-recon-postgres \
      psql -h 127.0.0.1 -U onprest_admin -d reconcile -Atc 'SELECT 1'
      ;;
    mysql)
    docker exec -e MYSQL_PWD=Onprest-admin-1 onprest-recon-mysql \
      mysql --connect-timeout=3 -h 127.0.0.1 -u root reconcile -Nse 'SELECT 1'
      ;;
    sqlserver)
    docker exec -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
      /opt/mssql-tools18/bin/sqlcmd -l 3 -S localhost -U sa -C -b -Q 'SELECT 1'
      ;;
    oracle)
    docker exec -i onprest-recon-oracle sqlplus -s -L system/Onprest-admin-1@localhost:1521/FREEPDB1 <<'SQL'
WHENEVER SQLERROR EXIT FAILURE
SELECT 1 FROM dual;
EXIT
SQL
      ;;
  esac
}
case "$db" in
  postgres)
    docker create --label "$label" --name onprest-recon-postgres \
      -p 127.0.0.1:55433:5432 \
      -e POSTGRES_DB=reconcile -e POSTGRES_USER=onprest_admin \
      -e POSTGRES_PASSWORD=Onprest-admin-1 postgres:16-alpine
    ;;
  mysql)
    docker create --label "$label" --name onprest-recon-mysql \
      -p 127.0.0.1:53306:3306 \
      -e MYSQL_DATABASE=reconcile -e MYSQL_ROOT_PASSWORD=Onprest-admin-1 \
  mysql:8.0.36
    ;;
  sqlserver)
    docker create --label "$label" --name onprest-recon-sqlserver --platform linux/amd64 \
      -p 127.0.0.1:51433:1433 \
      -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Onprest-admin-1 \
  mcr.microsoft.com/mssql/server:2022-CU14-ubuntu-22.04
    ;;
  oracle)
    docker create --label "$label" --name onprest-recon-oracle \
      -p 127.0.0.1:51521:1521 -e ORACLE_PASSWORD=Onprest-admin-1 \
  gvenzl/oracle-free:23-slim-faststart
    ;;
esac
created=true
    docker start "$name"
echo "Waiting for $db (up to five minutes)..."
deadline=$((SECONDS + 300))
until ready >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "Database readiness timed out" >&2
    exit 1
  fi
  sleep 2
done
case "$db" in
  postgres)
    docker exec -i onprest-recon-postgres \
      psql -v ON_ERROR_STOP=1 -U onprest_admin -d reconcile < schema.postgres.sql

    docker exec -i onprest-recon-postgres \
      psql -v ON_ERROR_STOP=1 -U onprest_admin -d reconcile <<'SQL'
CREATE ROLE capability_user LOGIN PASSWORD 'Onprest-example-1';
GRANT USAGE ON SCHEMA public TO capability_user;
GRANT SELECT, INSERT ON mutation_reconciliation_orders TO capability_user;
SQL
    ;;
  mysql)
    docker exec -i -e MYSQL_PWD=Onprest-admin-1 onprest-recon-mysql \
      mysql -u root reconcile < schema.mysql.sql

    docker exec -i -e MYSQL_PWD=Onprest-admin-1 onprest-recon-mysql \
      mysql -u root reconcile <<'SQL'
CREATE USER 'capability_user'@'%' IDENTIFIED BY 'Onprest-example-1';
GRANT SELECT, INSERT ON reconcile.mutation_reconciliation_orders TO 'capability_user'@'%';
SQL
    ;;
  sqlserver)
    docker exec -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
      /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b \
      -Q 'CREATE DATABASE reconcile'

    docker exec -i -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
      /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -d reconcile \
  < schema.sqlserver.sql

    docker exec -i -e SQLCMDPASSWORD=Onprest-admin-1 onprest-recon-sqlserver \
      /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -d reconcile <<'SQL'
CREATE LOGIN capability_user WITH PASSWORD = 'Onprest-example-1';
CREATE USER capability_user FOR LOGIN capability_user;
GRANT SHOWPLAN TO capability_user;
GRANT SELECT, INSERT ON OBJECT::dbo.mutation_reconciliation_orders TO capability_user;
GO
SQL
    ;;
  oracle)
    docker cp schema.oracle.sql onprest-recon-oracle:/tmp/schema.oracle.sql

    docker exec -i onprest-recon-oracle \
      sqlplus -s -L system/Onprest-admin-1@localhost:1521/FREEPDB1 <<'SQL'
WHENEVER SQLERROR EXIT FAILURE
@/tmp/schema.oracle.sql
CREATE USER capability_user IDENTIFIED BY "Onprest-example-1";
GRANT CREATE SESSION TO capability_user;
GRANT SELECT, INSERT ON system.mutation_reconciliation_orders TO capability_user;
CREATE SYNONYM capability_user.mutation_reconciliation_orders FOR system.mutation_reconciliation_orders;
EXIT
SQL
    ;;
esac
echo "$db ready: schema and capability_user initialized."
