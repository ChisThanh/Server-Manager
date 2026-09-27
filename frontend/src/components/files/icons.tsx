import {
  File,
  FileArchive,
  FileAudio,
  FileCode,
  FileCog,
  FileImage,
  FileJson,
  FileKey,
  FileSpreadsheet,
  FileTerminal,
  FileText,
  FileVideo,
  Database,
  Folder,
  FolderOpen,
} from "lucide-react";

const code = /\.(js|jsx|ts|tsx|mjs|cjs|go|py|rb|php|java|kt|c|h|cpp|hpp|cs|rs|swift|vue|svelte|html?|css|scss|less|sql|lua|pl|r|dart|scala|ex|exs)$/i;
const cfg = /\.(conf|cnf|ini|cfg|toml|ya?ml|env|service|timer|socket|properties|xml|plist)$|^(\.env.*|dockerfile|makefile|\.htaccess|nginx\.conf)$/i;

export function FileIcon({ name, isDir, open, size = 15 }: { name: string; isDir: boolean; open?: boolean; size?: number }) {
  if (isDir) {
    return open ? <FolderOpen size={size} color="#e8b44f" /> : <Folder size={size} color="#e8b44f" />;
  }
  const n = name.toLowerCase();
  if (/\.json$/.test(n)) return <FileJson size={size} color="#f5c451" />;
  if (code.test(n)) return <FileCode size={size} color="#6aa7ff" />;
  if (/\.(sh|bash|zsh|fish)$|^\.(bashrc|zshrc|profile|bash_profile)$/.test(n)) return <FileTerminal size={size} color="#34c77b" />;
  if (cfg.test(n)) return <FileCog size={size} color="#a0a0ab" />;
  if (/\.(png|jpe?g|gif|svg|webp|ico|bmp)$/.test(n)) return <FileImage size={size} color="#c678dd" />;
  if (/\.(zip|tar|gz|tgz|bz2|xz|7z|rar)$/.test(n)) return <FileArchive size={size} color="#e5a15a" />;
  if (/\.(pem|key|crt|cer|pub)$|^id_(rsa|ed25519|ecdsa)/.test(n)) return <FileKey size={size} color="#f0525d" />;
  if (/\.(csv|xlsx?)$/.test(n)) return <FileSpreadsheet size={size} color="#34c77b" />;
  if (/\.(mp4|mkv|mov|avi|webm)$/.test(n)) return <FileVideo size={size} color="#ff7ab6" />;
  if (/\.(mp3|wav|flac|ogg)$/.test(n)) return <FileAudio size={size} color="#ff7ab6" />;
  if (/\.(db|sqlite3?)$/.test(n)) return <Database size={size} color="#17c3b2" />;
  if (/\.(md|txt|log|rst)$|^readme/.test(n)) return <FileText size={size} color="#a0a0ab" />;
  return <File size={size} color="#8b8b94" />;
}
