# Changelog

The release workflow takes the notes for a version from the section below.

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
