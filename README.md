# Muriale's SDS Manager

A small Windows desktop program for Muriale's Italian Kitchen that keeps the
restaurant's Safety Data Sheet (SDS, formerly MSDS) library and prints the
paper SDS book.

## What it does

- Add, edit, replace and remove safety data sheets (manufacturer PDFs)
- One category per product from an editable list, plus optional tags, with
  manufacturer, supplier and storage location kept as separate fields
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

The GitHub Actions build also embeds the program icon (`app.ico`) by running
`windres -O coff -o rsrc_windows_amd64.syso app.rc` before the build.

## Credits

PDF assembly uses [pdf-lib](https://pdf-lib.js.org/) (MIT licence), bundled as
`ui/pdf-lib.min.js`.

## Version 1.4

- **Shared library, one editor at a time.** The exe and the `SDS Data` folder can sit on a OneDrive, SharePoint or network folder and be used from several computers. The first copy opened can make changes. Others open view-only (search, view, print) and see who is editing. The lock (`edit.lock`) is refreshed every 20 seconds and released when the program closes; a lock nobody has refreshed for 3 minutes is treated as abandoned. "Take Over" is there for a crashed computer.
- **Settings tabs:** Storage, Backup & Restore, Import & Export, Clean Up, Recovery, Diagnostics.
- **Aliases** ("Also Known As") are searchable. **Handoff notes** for the next manager. **Emergency Sheet**: a one-page printout of a product's summary.
- **Document integrity:** each PDF is fingerprinted when added; a changed file is flagged and keeps a book from being called complete until a manager accepts or replaces it.
- **Originals stay unchanged.** Previews show the stored PDF as is. Printed books copy the manufacturer's pages as they are; binder page numbers are optional and off by default. The index always lists the page each SDS starts on. "Printer-friendly" outlines the bars instead of filling them.
- **Import/Export:** product list as CSV, whole library as a zip, bulk PDF import, library-zip import.
- **Diagnostics and Recovery:** health checks with a saveable report; go back one save, rebuild from the PDFs in the folder, add back old PDFs.
