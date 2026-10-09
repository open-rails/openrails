// The extensions this build carries. Standalone has none; a host build
// (scripts/build-admin-console.sh --extensions <module>) replaces this file
// with a re-export of the host's module.
import type { ConsoleExtension } from "./types"

const extensions: ConsoleExtension[] = []

export default extensions
