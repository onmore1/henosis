# Hénōsis

Shared FL Studio projects without zipping and sending. You save in FL Studio,
your friend's folder updates by itself. One person edits at a time; if two
people do change the same file, both versions are kept, and every version has
a history you can comment on.

Windows only for now. Version 0.5.0.

## Where to get it

Every version is on the repository's [Releases
page](https://github.com/onmore1/henosis/releases): download **Henosis.exe**
and double-click it, or take the zip if you also want the command-line tools,
the Linux server and the source.

To publish a new one: open the **Actions** tab, pick **Release**, press **Run
workflow** and type the version (for example `0.4.1`). GitHub runs the tests,
builds the Windows programs, makes the tag and puts the release up. Its notes
come from that version's section in `CHANGELOG.md`.

## What is in a release

```
Henosis.exe             the app. Double-click it. Everything is in here
tools/                  optional: the same things as command-line programs
  henosis-cli.exe         "henosis-cli help"
  henosis-server.exe      the server on its own, for a machine without the app
  henosis-server-linux    the same for a Linux machine
henosis-source.zip      source of everything (Go, no dependencies)
```

## Set it up (two clicks each)

**You (the one who starts the session):**

1. Double-click `Henosis.exe`. Its window opens.
2. Type your name, press **Start a session**. If Windows asks whether to
   allow Henosis on your network, say yes.
3. Press **Copy code** and send the code to your friend however you like
   (Telegram, Discord, anything).
4. Add a project: press **Add a project** and pick the folder that holds the
   `.flp`, or just drag that folder onto the window from File Explorer. The
   projects already in your FL Studio projects folder are offered as chips,
   one click each.

**Your friend:**

1. Double-click `Henosis.exe`.
2. Type their name, paste the code, press **Join**.

That is it. Your projects appear on their computer in
`Documents\Henosis\<project>`, and anything either of you adds later shows up
on the other side by itself. From then on: open the `.flp` from that folder,
work, save.

Closing the window does not stop the app; it keeps syncing. Start
`Henosis.exe` again to get the window back. "Quit Hénōsis" at the bottom
stops it. "Start with Windows" makes it start by itself.

The session lives on the computer that pressed Start: that computer has to be
on, with Henosis running, for the others to sync.

## Taking a project out

Two different things, on each project's card:

- **Stop syncing here** unlinks that folder from this computer. The files stay
  where they are, the project stays in the session, and you can download it
  again later from "In the session, not on this computer".
- **Remove from Hénōsis** (only on projects you added) takes the project out
  of the session for everyone. Its versions and comments are deleted from the
  server; nobody's files are touched. Your friend's app then says the project
  is no longer available and offers to take it off their list.

## The debug console

It sits at the bottom of the window and shows what the app does as it happens:
every button you press (lines starting with `>`), every upload and download,
when FL Studio opens or closes a project and which window titles it saw, lock
changes, and every error in red.

Drag the strip along its top edge to make it taller or shorter, or press Hide
to put it away; the size and the state are remembered. "show details" adds the
fine grain (each request, timings), and "Copy" puts the whole console on the
clipboard. The same lines go to `%AppData%\Henosis\agent.log`. Tokens and
session codes are never written to either.

## History, notes and comments

The right-hand side of the window shows the history of the project you have
selected (click a project card to switch).

- **Note for your next save:** type one and press Set, and the next save is
  recorded under that text, like a commit message. Without a note the app
  describes the change itself ("Changed My Song.flp").
- **Comments:** pick a version, write, Send. Everyone in the session sees them.
- **Restore** brings an older version back as the newest one. Nothing is
  rewritten; the restore is itself a new version.
- Old versions are kept 3 days by default; the project's owner can change that
  on the project card. Comments disappear together with their version.

## When the code does not connect

The code contains your computer's addresses on its networks. It works when
you are on the same Wi-Fi or LAN, or when you both run Tailscale (or a
similar private network). Under "It does not connect? Addresses and options"
you can see the addresses it carries and add one by hand — for example a
public IP or a domain if you forwarded port 8787 on your router. Send the new
code after adding one.

"Make a new code" locks out anyone who has the old code; everyone already
connected stays connected.

The connection is plain HTTP, so what travels is unencrypted. That is fine on
your own network or over Tailscale; do not forward the port to the open
internet.

If you want a session that is up even when your computer is off, run
`tools/henosis-server-linux serve` on a small Linux server, make accounts
there with `henosis-server adduser NAME`, and use "Advanced: connect with a
server address and a token" on the first screen.

## How it behaves

- **Saving:** the app waits until FL Studio has finished writing (about two
  seconds), then uploads. A file is uploaded once however many versions use it.
- **Working together:** when you open a shared project in FL Studio, the app
  takes the project's lock. Your friend sees "you are editing". When you close
  the project, the lock is released.
- **Opening while a friend edits:** you can look around. Your saves stay on
  your computer. When your friend is done, close the project and it updates;
  if you saved in the meantime, your version is kept next to theirs as
  `name (conflict, you, date).flp`.
- **A project open in FL Studio is never changed underneath it.** Updates wait
  until you close the project.
- **A computer that vanished while editing:** its lock expires after 10
  minutes. The owner can also break it from the project card.
- **Solo projects** need no lock and no friend: it is a backup with history,
  and it syncs between your own computers.
- **Samples outside the project folder** do not sync. The project card lists
  them and "Copy them into the project" copies them next to the `.flp`, where
  FL Studio looks for samples it cannot find at their original place.
- **Safety:** a missing folder is never treated as "delete everything"; a
  change that removes more than half of the files asks first; files replaced
  by an update are kept 7 days in the hidden `.henosis\trash` folder.
- Not synced: `Backup\` (FL Studio's own backups), temporary files, and
  `.henosis\`.

## What was tested, and what was not

Everything below runs with `go test ./...` in `app/`:

- the sync rules, against a real server over HTTP, including interrupted
  uploads and downloads, lost answers, and 600 runs of random edits on two
  folders;
- editing sessions with two simulated computers: locks, a second editor,
  takeover of a vanished computer, handing over, offline work, restart;
- a whole session end to end: start, join by code, projects appearing by
  themselves, suggestions, notes, comments, a new code;
- the three real programs as separate processes, and a newer app replacing an
  older one that is still running;
- the project-file reader against two real FL Studio 2025 projects.

On top of that, `app/uitest/ui_flow.py` drives two app windows in a real
browser like two people: typing slowly, pressing buttons, checking that
nothing is wiped and that both sides see each other's work.

Beyond that, version 0.3.1 was run on a real Windows 11 machine with FL
Studio 2025, where the following was checked by hand: the app opens in a
window of its own; a session starts on port 8787 and shows its code; the
projects found in `Documents\Image-Line\FL Studio\Projects` are offered for
one click; a project uploads; projects already in the session download by
themselves; opening a project in FL Studio is noticed (even when the window
shows the project's title instead of its file name) and takes the lock;
Ctrl+S in FL Studio uploads the save within seconds; closing FL Studio gives
the lock back; and `.henosis` stays hidden.

Three things have still never run anywhere: a second computer joining a
session over a network (it was tested with two apps on one machine), "Start
with Windows", and the app window on a computer without Edge or Chrome, where
it falls back to a browser tab.

If FL Studio detection ever fails, the debug console says so: when you open a
project a line "opened in FL Studio (windows: ...)" must appear within a
second or two. If it does not, sync still works on every save, but the lock is
only taken for the moment of each upload.

Known limits: a `.flp` with audio recorded inside it (Edison, Slicex) is
uploaded whole on every save; there is no tray icon and no installer yet; the
app window needs Edge or Chrome to look like a window; the token is stored in
`%AppData%\Henosis\config.json` readable by your Windows user; if a friend
uploads in the same seconds in which you are opening the project, FL Studio
may have loaded the version from just before.

## Build from source

Install Go 1.23 or newer, unpack `henosis-source.zip`, then in `app/`:

```
go test ./...
go build -ldflags "-H=windowsgui" -o Henosis.exe ./cmd/henosis-agent
go build -o henosis-cli.exe ./cmd/henosis
go build -o henosis-server.exe ./cmd/henosis-server
```
