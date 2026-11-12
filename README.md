# Muriale's SDS Manager

A small Windows desktop program for Muriale's Italian Kitchen that keeps the
restaurant's Safety Data Sheet (SDS, formerly MSDS) library and prints the
paper SDS book.

## What it does

- Add, edit, replace and remove safety data sheets (manufacturer PDFs)
- Search by product or manufacturer, filter by category, sort any column
- Emergency quick view per product: hazards, first aid, protective equipment
- Print the entire book, selected sheets, one sheet, or the index only, with
  optional cover page, index, category dividers and page numbers
- Manage categories (their order sets the section order of the printed book)
- One-click backup of the whole library to a zip file

## How it works

The program is a single `.exe` with no installer. It runs a private server
that only the same computer can reach and shows its screens in an application
window using Microsoft Edge, which is part of Windows.

The library is file-first: plain files in a folder, with no SQL or SQLite
database. All data stays on the computer, in an `SDS Data` folder next to the
program:

| Path | Contents |
| --- | --- |
| `SDS Data/library.json` | Product list and categories |
| `SDS Data/sheets/` | The SDS PDF files, under readable names |
| `SDS Data/removed/` | Removed and replaced PDFs, kept for recovery |

A PDF copied by hand into `SDS Data/sheets/` is picked up as a new product the
next time the library is shown.

## Starter sheets

Sheets in `starter/` are built into the program. Each is copied into the
library once, the first time a build containing it runs. To ship another one,
add its PDF to `starter/sheets/` and its details to `starter/library.json`.

## Building

Requires Go 1.22 or newer. No other dependencies.

    GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H=windowsgui" -o MurialesSDSManager.exe .

## Credits

PDF assembly uses [pdf-lib](https://pdf-lib.js.org/) (MIT licence), bundled as
`ui/pdf-lib.min.js`.
