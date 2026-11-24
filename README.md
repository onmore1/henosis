# Hénōsis

Make FL Studio beats with a friend on two computers, with no zipping and
sending. You press Ctrl+S, your friend's copy updates by itself. One person
edits at a time, every save is a version you can go back to, and your samples
travel with the project.

Windows 10 and 11. Tested with FL Studio 2025. Free.

## Get it

1. Download **Henosis.exe** from the [latest
   release](https://github.com/onmore1/henosis/releases/latest).
2. Double-click it. Windows may say **"Windows protected your PC"**, because
   the app is new and not code-signed yet: click **More info**, then **Run
   anyway**.
3. If Windows asks whether Hénōsis may use your network, click **Allow**.
   (It lets a friend on the same Wi-Fi connect directly; over the internet
   it works either way.)

There is nothing to install and no account to make. The app checks for new
versions and shows a **Download** button when one is out; everyone in a
session should run the same version.

## Make a beat together

**You:**

1. Open Hénōsis, type your name, press **Start a new session**.
2. Press **Copy invite code** and send it to your friend (Discord, Telegram,
   anything).
3. Press **Add a project** and pick the folder with your `.flp`, or drag the
   folder onto the window.

**Your friend:** opens Hénōsis, types their name, pastes the code, presses
**Join**. The project appears in their `Documents\Henosis` folder.

Then just work: open the `.flp` from that folder, make changes, Ctrl+S. While
one of you has the project open, the other sees "is editing" and can ask for
a turn.

The session lives on the computer that started it: that computer has to be on
with Hénōsis running for the others to sync. Closing the window does not stop
the app; **Quit Hénōsis** at the bottom does.

## How you connect, and what anyone else can see

Hénōsis first tries to reach your friend's computer directly (same Wi-Fi, or
a private network like Tailscale). When that is not possible, it goes through
the Hénōsis relay, a small service on Cloudflare that only passes bytes
between the two apps:

- Everything that goes through the relay is **encrypted end to end**
  (AES-256-GCM, with keys made from the invite code). The relay cannot read
  your projects or change them.
- The relay is never told the invite code, only a hash of it, so it cannot
  join your session either.
- Nothing is stored on the relay and nothing is logged.
- Your projects, versions and comments live on the computer that started the
  session, not in a cloud.

Treat the invite code like a password: anyone who has it can join. **Make a
new code** under Session locks out anyone with the old one.

The relay runs on Cloudflare's free plan, which has a daily allowance shared
by everyone. If it is used up, the app says so; it resets at midnight UTC,
and friends on the same network or on Tailscale are not affected.

## When something goes wrong

- **"Report a problem"** at the bottom of the window opens a GitHub issue
  with your version filled in. If the app crashed, attach `crash.log` from
  `%AppData%\Henosis`.
- The **Console** strip at the bottom shows what the app is doing; **Copy**
  puts it on the clipboard for a report. Invite codes and tokens never appear
  in it.
- The friend cannot connect: check that the host's Hénōsis is running and
  that you both have the same version.
- Your antivirus flags the exe: Go programs that are not code-signed are
  sometimes flagged by mistake. The source is public in this repository,
  and you can check the file on VirusTotal.

## Everything else

### Taking a project out

Two different things, on each project's card:

- **Stop syncing here** unlinks that folder from this computer. The files stay
  where they are, the project stays in the session, and you can download it
  again later from "In the session, not on this computer".
- **Remove from Hénōsis** (only on projects you added) takes the project out
  of the session for everyone. Its versions and comments are deleted from the
  server; nobody's files are touched. Your friend's app then says the project
  is no longer available and offers to take it off their list.

### The debug console

It sits at the bottom of the window and shows what the app does as it happens:
every button you press (lines starting with `>`), every upload and download,
when FL Studio opens or closes a project and which window titles it saw, lock
changes, and every error in red.

Drag the strip along its top edge to make it taller or shorter, or press Hide
to put it away; the size and the state are remembered. "show details" adds the
fine grain (each request, timings), and "Copy" puts the whole console on the
clipboard. The same lines go to `%AppData%\Henosis\agent.log`. Tokens and
session codes are never written to either.

### History, notes and comments

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

### How it behaves

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

## Known limits

- Windows only. The app window needs Edge or Chrome (both come with Windows).
- One person edits a project at a time; a save made while a friend edits is
  kept as a conflict copy, never lost.
- A `.flp` with audio recorded inside it (Edison, Slicex) is uploaded whole
  on every save.
- No installer and no tray icon yet; the exe is the whole app.

## Build from source

Install Go 1.23 or newer, unpack `henosis-source.zip`, then in `app/`:

```
go test ./...
go build -ldflags "-H=windowsgui" -o Henosis.exe ./cmd/henosis-agent
go build -o henosis-cli.exe ./cmd/henosis
go build -o henosis-server.exe ./cmd/henosis-server
```

The relay is in `worker/` (a Cloudflare Worker); see its README to run your
own. `go run ./cmd/relaycheck <relay URL>` checks a relay end to end.

To publish a new version: open the **Actions** tab, pick **Release**, press
**Run workflow** and type the version (for example `0.8.1`). GitHub runs the
tests, builds the Windows programs, makes the tag and puts the release up,
with the notes from that version's section in `CHANGELOG.md`.

## License

See [LICENSE](LICENSE).
