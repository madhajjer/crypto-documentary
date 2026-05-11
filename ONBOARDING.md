---
name: Junior onboarding handoff
description: Human-friendly guide to run mm-bot for someone new to Go and Vue. Focuses on the happy path: clone → build → run → observe.
---

# Welcome to mm-bot! 🤖

You're joining a team that runs a **market maker bot** on Indodax (an Indonesian crypto exchange). This bot automatically places buy/sell orders to profit from the bid-ask spread. Your job: learn to build it, run it, and keep it running.

**Good news:** You don't need to understand the math yet. You just need to follow steps.

---

## What is mm-bot?

- **Backend:** A Go program (think: a compiled command-line app, not a website).
- **Frontend:** A Vue dashboard (think: a web page that shows what the bot is doing).
- **Together:** They watch the market, place orders, and show you real-time status in a browser.

**You won't code Go or Vue today.** You'll just run what already exists.

---

## Prerequisites (One-time setup)

### 1. Install Go

Go is a programming language. The bot is written in it.

1. Go to https://golang.org/dl
2. Download the latest version (e.g., `go1.22.1`)
3. Run the installer. Accept all defaults.
4. Open a **new** PowerShell terminal (important: new, so it picks up the install).
5. Verify it worked:
   ```powershell
   go version
   ```
   You should see something like `go version go1.22.1 windows/amd64`. If you see an error, restart your computer.

### 2. Install Node.js

Node.js runs the dashboard build process.

1. Go to https://nodejs.org
2. Download the LTS (Long Term Support) version.
3. Run the installer. Accept all defaults.
4. Open a **new** PowerShell terminal.
5. Verify:
   ```powershell
   node --version
   npm --version
   ```

### 3. Clone the repository

```powershell
cd $env:USERPROFILE\Desktop          # or wherever you want to work
git clone https://github.com/madhajjer/mm-bot.git
cd mm-bot
```

---

## Build the bot (One-time)

```powershell
cd d:\ahmad-muhajir\_repo\trading-bot\mm-bot
go build ./...
```

**What this does:** Compiles Go code into a `.exe` file. Takes ~30 seconds. You'll see no output if it succeeds. If you see errors, stop and ask.

**Where's the binary?** Look for `bot.exe` in the `cmd/bot/` folder. (Or just `./bot` if you're in PowerShell.)

---

## Build the dashboard (One-time or after frontend changes)

```powershell
cd web                                 # go into the web folder
npm install                            # downloads ~2000 tiny libraries; takes 1–2 min
npm run build                          # compiles Vue → static HTML/CSS/JS; takes 10 sec
cd ..                                  # back to mm-bot root
```

**What this does:** Packages the Vue dashboard into plain HTML/CSS/JS that a browser can load. The Go bot serves these files.

---

## Create a `.env` file (Credentials)

The bot needs your Indodax API key and secret to place real trades.

1. In the `mm-bot` root folder, create a file called `.env` (no extension, just a dot).
2. Paste this:
   ```
   INDODAX_API_KEY=your_api_key_here
   INDODAX_API_SECRET=your_api_secret_here
   MM_PAIR=btc_idr
   MM_GAMMA=0.1
   MM_KAPPA=0.5
   ```

3. Replace `your_api_key_here` and `your_api_secret_here` with your actual Indodax credentials (ask your team lead).

**⚠️ IMPORTANT:** This file has your secrets. **NEVER commit it to git.** It's in `.gitignore`, so git will ignore it automatically — don't try to add it.

---

## Run the bot

### Terminal 1: Start the bot backend

```powershell
cd d:\ahmad-muhajir\_repo\trading-bot\mm-bot
go run ./cmd/bot
```

**What you should see:**
```
[INFO] MMController: Starting market maker for btc_idr...
[INFO] WebSocket: Connected to Indodax public stream
[INFO] Bot: Listening on http://localhost:8080
```

This means the bot is running and waiting for a browser to connect.

**If you see an error:**
- `"address already in use"` → Port 8080 is taken. Stop any other bot running, or edit `internal/server/config.go` to use a different port.
- `"API key not found"` → Your `.env` file is missing or wrong. Check step above.
- `panic` or `fatal` → The bot crashed. Ask for help, but first check the error message.

### Terminal 2: Start the dashboard dev server

Open a **new** PowerShell terminal in the same folder:

```powershell
cd d:\ahmad-muhajir\_repo\trading-bot\mm-bot\web
npm run dev
```

**What you should see:**
```
  ➜  Local:   http://localhost:5173/
```

The dashboard is now running on a separate port (5173).

### Terminal 3: Open a browser

1. Open Chrome, Firefox, or Edge.
2. Go to `http://localhost:5173/`
3. You should see a dashboard with:
   - Current bid/ask prices
   - Your open orders
   - P&L (profit/loss)
   - Real-time charts

**If the page is blank or shows errors:**
- Check that Terminal 1 is still running (the backend must be alive).
- Refresh the page (Ctrl+R or Cmd+R).
- Open the browser console (F12 → Console tab) and look for error messages.

---

## Typical workflow

1. **Terminal 1:** `go run ./cmd/bot`
2. **Terminal 2:** `cd web && npm run dev`
3. **Browser:** Open `http://localhost:5173/`
4. **Watch:** Observe orders being placed and filled in real-time.
5. **Stop:** Press `Ctrl+C` in both terminals to shut down gracefully.

---

## If something breaks

### "Port 8080 already in use"
Another bot is running on the same machine. Either:
- Kill it: `Get-Process | Where-Object {$_.Name -eq "bot"} | Stop-Process`
- Or wait 10 seconds and try again (sometimes the OS needs time to release it).

### "npm ERR! Cannot find module 'vue'"
You skipped `npm install`. Go to the `web` folder and run it again:
```powershell
cd web && npm install && cd ..
```

### "GO111MODULE=on" or module errors
Outdated Go. Update Go to the latest version and try again.

### Dashboard shows "Cannot connect to backend"
Terminal 1 (the bot) is not running. Make sure it's still going in its terminal. If it crashed, scroll up in Terminal 1 to see the error.

### Bot says "Permission denied" or "Invalid API key"
Your `.env` credentials are wrong or missing. Double-check them with your team lead.

---

## What to explore next

Once you've run it a few times and it feels comfortable:

1. **Read the code:** Start with `cmd/bot/main.go`. It's the entry point; ~100 lines.
2. **Understand the strategy:** Open `internal/hfmm/strategy/avellaneda.go`. This is where the math lives (but you don't need to change it).
3. **Watch the logs:** The bot prints status every cycle. What do the fields mean?
4. **Modify a parameter:** Change `MM_GAMMA` in `.env` from 0.1 to 0.05, restart, and see if spread changes.
5. **Run tests:** `go test ./...` will run automated tests to make sure nothing broke.

---

## Asking for help

If you're stuck:
1. **Search the error message** in the project's GitHub issues.
2. **Check the README** at the root of the repo.
3. **Ask your team lead** with details:
   - What were you trying to do?
   - What error did you see (copy the full message)?
   - What does `go version` and `node --version` output?

---

## Cheat sheet (copy-paste)

**First time only:**
```powershell
go version                              # check Go
node --version && npm --version         # check Node
cd $env:USERPROFILE\Desktop
git clone https://github.com/madhajjer/mm-bot.git
cd mm-bot
go build ./...
cd web && npm install && cd ..
# Then create .env with your credentials
```

**Every time you want to run:**
```powershell
# Terminal 1
cd d:\ahmad-muhajir\_repo\trading-bot\mm-bot
go run ./cmd/bot

# Terminal 2 (new PowerShell window)
cd d:\ahmad-muhajir\_repo\trading-bot\mm-bot\web
npm run dev

# Terminal 3 (browser)
http://localhost:5173/
```

**To stop:**
```powershell
Ctrl+C                                 # in each terminal
```

---

**You've got this.** Take your time, and don't hesitate to ask questions.
