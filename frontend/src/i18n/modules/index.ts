// Feature-module dictionaries, merged into every language by i18n/index.ts.
import app from "./app";
import mon from "./mon";
import docker from "./docker";
import logs from "./logs";
import sec from "./sec";
import web from "./web";
import db from "./db";
import backup from "./backup";
import deploy from "./deploy";
import cmd from "./cmd";

export const MODULES = {
  app,
  mon,
  docker,
  logs,
  sec,
  web,
  db,
  backup,
  deploy,
  cmd,
};
