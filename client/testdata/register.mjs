// Registers ts_resolve_hook.mjs for a node run started with
// --import ./register.mjs from this directory.
import { register } from "node:module";

register(new URL("./ts_resolve_hook.mjs", import.meta.url));
