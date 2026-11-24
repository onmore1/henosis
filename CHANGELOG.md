# Changelog

The release workflow takes the notes for a version from the section below.

## 0.8.1

The first version for everyone: download it, double-click, connect.

- **Works out of the box.** The Hénōsis relay is built in, so a friend
  anywhere connects with just the invite code, no Tailscale and no setup.
- **Encrypted end to end.** Everything that passes through the relay is
  encrypted with keys made from the invite code, and the relay is only told a
  hash of the code. It can neither read your projects nor join your session.
  Both of you need 0.8.1 or newer to connect through the relay.
- **Tells you when there is a new version**, with a Download button.
- **Report a problem** at the bottom of the window opens a GitHub issue with
  your version filled in, in your default browser (the Download button too).
- **Clearer errors when joining**: the app says whether the relay is out of
  its daily allowance, your friend runs another version, or nobody is there.
- The relay carries many more people on the same free plan: waiting
  connections are renewed every 10 minutes instead of 2, dead ones are found
  by their missing ping answers, data goes in larger pieces, and a relay that
  is down or full is retried less and less often instead of every 3 seconds.
- **A new download always takes over** from the copy already running. It used
  to give up when the running copy was slow to answer (its friend's computer
  gone), and the old version kept running.
- The window no longer freezes for up to 20 seconds when the session's
  computer cannot be reached.
- `HENOSIS_FORCE_RELAY=1` makes the app skip direct addresses when joining,
  to test the relay from one network.

## 0.7.5

- **The logo is the app's icon**: on Henosis.exe in File Explorer, on the
  window, and on its taskbar button.
- After an update, an open Hénōsis window reloads by itself, so it shows the
  new version instead of the old one until it is reopened.

## 0.7.3

- **No more endless "a newer version arrived first".** A project folder that
  was synced in an earlier session kept that session's sync state, so when a
  project of the same name in a new session used the same folder, Hénōsis
  retried every few seconds and never synced. The old state is now set aside;
  nothing is lost, and a file that differs is kept as a conflict copy.

## 0.7.2

- **Samples are found by name**, like FL Studio does, when the path stored
  in the project no longer matches the folders on disk (a pack re-extracted
  or renamed after you used it). Such a sample used to count as missing, so
  it never travelled with the project.

## 0.7.1

- **A new session starts clean.** "Start a new session" is now the main
  button on the start screen (and what Enter does). The session already on
  the computer is listed underneath, with who is in it, if you want to
  continue it instead.
- **Joining a session starts clean too.** Projects from a session you were in
  before no longer follow you in as "No longer available"; their folders and
  files stay where they are.
- **Samples travel by themselves.** A sample a project uses from elsewhere on
  your computer (a pack in Downloads, say) is copied next to the project file,
  where FL Studio looks for it, and sent with the project. No button to
  remember. A sample that is only on your friend's computer shows as "not on
  this computer yet" and arrives once Hénōsis runs there.
- **A screen while connecting** says what is happening: looking for your
  friend's computer, going through the relay, signing you in.
- When you start a session, the window says what Windows' network question
  means. Allow is best; Cancel still works, through the relay.
- If Hénōsis ever crashes, the reason is written to
  `%AppData%\Henosis\crash.log`.

## 0.7.0

Download **Henosis.exe** and double-click it.

- **Connect from anywhere.** Your friend no longer needs to be on your Wi-Fi
  or on Tailscale. When no direct address works, the two apps meet through a
  small relay on Cloudflare. Nothing to set up: paste the code and press Join.
  Your friend's bar says whether you are connected directly or through the
  relay.
- **A calmer window.** The session is a bar across the top: whether
  everything is synced, who is in, how you are connected, and the invite code
  with a copy button. Each project shows its status and one main button,
  **Open in FL Studio**; the rest (show in File Explorer, sync now, stop
  syncing, remove) is in its ⋯ menu.
- **Send and update by hand.** Turn off Auto-send or Auto-update on a project
  and its **Send changes** or **Update now** button appears next to it.
- **Three color themes**: dark, light and teal, from the dots at the top
  right. The window remembers your pick.
- The console is a one-line strip at the bottom that shows what the app last
  did and counts new errors. Click it to open the full log.

## 0.5.0

Download **Henosis.exe** and double-click it.

- **One way to add a project.** Press **Add a project** and pick the folder
  that holds the `.flp`, or drag that folder onto the window from File
  Explorer. A browser never tells a page where a folder is on disk, so the app
  looks for it under your Documents, Desktop, Downloads and the rest of your
  home folder, matching the folder's name against the files inside it. It is
  linked where it really lives, never copied — except a lone `.flp` sitting in
  your FL Studio projects folder, which gets a folder of its own.
- A folder it cannot find, or something that is not a project, says so instead
  of guessing. The path box is still there under "Paste a path instead".

## 0.4.0

Download **Henosis.exe** and double-click it. That is the whole app: the sync,
the server and the window are inside it. The zip is the same thing plus the
command-line tools, the Linux server and the source.

Set up a session: type your name, press **Start a session**, press **Copy
code** and send the code to your friend. They open Hénōsis, type their name,
paste the code, press **Join**. Projects then appear on both computers by
themselves.

New in this version:

- **Remove from Hénōsis** takes a project out of the session for everyone;
  nobody's files are deleted. It is separate from **Stop syncing here**, which
  only unlinks the folder on your own computer.
- The debug console can be dragged taller or shorter, and remembers its size.
- Clearer first screen, help text and tooltips; the example folder path no
  longer loses its backslashes.

Fixed in 0.3.1, after testing on a real Windows machine:

- A session runs on port 8787 instead of a random one.
- Starting a session with a name that already exists on your own computer
  signs you back in instead of refusing it.
- The `.henosis` folder inside a project is hidden again.

Verified on Windows 11 with FL Studio 2025: opening a project is detected and
takes the lock, Ctrl+S uploads within seconds, and closing FL Studio releases
the lock.
