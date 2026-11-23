# Changelog

The release workflow takes the notes for a version from the section below.

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
