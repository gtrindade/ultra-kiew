# Ultra Kiew

Telegram bot using AI to do silly things.

## How to run it

### Database
Make sure you have MySQL installed and running. Then, execute the following commands to set up the database:
```bash
sudo mysql -u root <<'SQL'
CREATE DATABASE dndtools;
CREATE USER 'dndtools'@'localhost' IDENTIFIED BY 'strong_password_here';
GRANT ALL PRIVILEGES ON dndtools.* TO 'dndtools'@'localhost';
FLUSH PRIVILEGES;
SQL
```
```bash
mysql -u dndtools -pstrong_password_here dndtools < data/dnd-35.sql
```

```bash
sudo mysql -u root <<'SQL'
CREATE DATABASE srd;
CREATE USER 'srd'@'localhost' IDENTIFIED BY 'strong_password_here';
GRANT ALL PRIVILEGES ON srd.* TO 'srd'@'localhost';
FLUSH PRIVILEGES;
SQL
```
```bash
mysql -u srd -pstrong_password_here srd < data/srd-db-v1.3.sql
```

Create a config.yaml file like this:

```yaml
telegram_bot_token: "<your-telegram-token>"
gemini_api_key: "<your-gemini-key>"
bot_name: "ultra-kiew"
dnd_tools:
  host: "localhost"
  port: 3306
  user: dndtools
  password: strong_password_here
  database: dndtools
srd:
  host: "localhost"
  port: 3306
  user: srd
  password: strong_password_here
  database: srd
```

### Start the bot
Run the following command to start the bot:
```bash
go run main.go
```
