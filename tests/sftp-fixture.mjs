import ssh2 from "ssh2";
const { STATUS_CODE: S, OPEN_MODE: F } = ssh2.utils.sftp;
// `files` maps absolute paths to {data, mode} for files and {dir: true} for
// directories. Directories are implied for ancestors of any known path.
export function attachSFTP(session, files, control = {}) {
  session.on("sftp", (accept) => {
    const sftp = accept(),
      handles = new Map();
    let next = 0;
    sftp.on("end", () => sftp.end());
    const isDir = (path) =>
      path === "/" ||
      files.get(path)?.dir ||
      [...files.keys()].some((p) =>
        p.startsWith(path.replace(/\/$/, "") + "/"),
      );
    const attrs = (path) => {
      const file = files.get(path);
      if (isDir(path))
        return {
          mode: (file?.mode ?? 0o755) | 0o040000,
          uid: 501,
          gid: 20,
          size: 4096,
          atime: 0,
          mtime: Math.floor((file?.mtime ?? 1700000000000) / 1000),
        };
      return {
        mode: file.mode || 0o100600,
        uid: 501,
        gid: 20,
        size: file.data.length,
        atime: 0,
        mtime: Math.floor((file.mtime ?? 1700000000000) / 1000),
      };
    };
    const exists = (path) => files.has(path) || isDir(path);
    const handleFor = (value) => {
      const handle = Buffer.alloc(4);
      handle.writeUInt32BE(++next);
      handles.set(next, value);
      return handle;
    };
    sftp.on("REALPATH", (id, path) => {
      let resolved = path === "." ? "/home/testuser" : path;
      resolved = resolved.replace(/\/+/g, "/").replace(/(.)\/$/, "$1");
      if (!exists(resolved)) return sftp.status(id, S.NO_SUCH_FILE);
      sftp.name(id, [
        { filename: resolved, longname: resolved, attrs: attrs(resolved) },
      ]);
    });
    sftp.on("LSTAT", (id, path) =>
      exists(path)
        ? sftp.attrs(id, attrs(path))
        : sftp.status(id, S.NO_SUCH_FILE),
    );
    sftp.on("STAT", (id, path) =>
      exists(path)
        ? sftp.attrs(id, attrs(path))
        : sftp.status(id, S.NO_SUCH_FILE),
    );
    sftp.on("OPENDIR", (id, path) => {
      if (!isDir(path)) return sftp.status(id, S.NO_SUCH_FILE);
      sftp.handle(id, handleFor({ dir: path, sent: false }));
    });
    sftp.on("READDIR", (id, handle) => {
      const state = handles.get(handle.readUInt32BE());
      if (!state || state.dir === undefined) return sftp.status(id, S.FAILURE);
      if (state.sent) return sftp.status(id, S.EOF);
      state.sent = true;
      const prefix = state.dir === "/" ? "/" : state.dir + "/";
      const names = new Set();
      for (const p of files.keys())
        if (p.startsWith(prefix) && p !== prefix)
          names.add(p.slice(prefix.length).split("/")[0]);
      sftp.name(
        id,
        [...names].map((filename) => {
          const full = prefix + filename;
          return { filename, longname: filename, attrs: attrs(full) };
        }),
      );
    });
    sftp.on("OPEN", (id, path, flags) => {
      if (isDir(path)) return sftp.status(id, S.FAILURE);
      if (files.has(path) && flags & F.EXCL) return sftp.status(id, S.FAILURE);
      if (!(flags & F.CREAT) && !files.has(path))
        return sftp.status(id, S.NO_SUCH_FILE);
      if (!files.has(path))
        files.set(path, { data: Buffer.alloc(0), mode: 0o100600 });
      sftp.handle(id, handleFor(path));
    });
    sftp.on("READ", (id, handle, offset, length) => {
      const file = files.get(handles.get(handle.readUInt32BE()));
      if (!file || file.dir) return sftp.status(id, S.FAILURE);
      if (offset >= file.data.length) return sftp.status(id, S.EOF);
      sftp.data(id, file.data.subarray(offset, offset + length));
    });
    sftp.on("WRITE", (id, handle, offset, data) => {
      const path = handles.get(handle.readUInt32BE()),
        file = files.get(path);
      if (!file) return sftp.status(id, S.FAILURE);
      const bytes = Buffer.alloc(
        Math.max(file.data.length, offset + data.length),
      );
      file.data.copy(bytes);
      data.copy(bytes, offset);
      file.data = bytes;
      if (control.delay)
        setTimeout(() => {
          if (!sftp.destroyed) sftp.status(id, S.OK);
        }, control.delay);
      else sftp.status(id, S.OK);
    });
    sftp.on("FSETSTAT", (id, handle, attributes) => {
      const file = files.get(handles.get(handle.readUInt32BE()));
      if (file) {
        if (attributes.mode) file.mode = attributes.mode;
        sftp.status(id, S.OK);
      } else sftp.status(id, S.FAILURE);
    });
    sftp.on("CLOSE", (id, handle) => {
      handles.delete(handle.readUInt32BE());
      sftp.status(id, S.OK);
    });
    sftp.on("MKDIR", (id, path) => {
      if (exists(path)) return sftp.status(id, S.FAILURE);
      files.set(path, { dir: true, mode: 0o755 });
      sftp.status(id, S.OK);
    });
    sftp.on("RMDIR", (id, path) => {
      if (!files.get(path)?.dir) return sftp.status(id, S.FAILURE);
      const prefix = path + "/";
      if ([...files.keys()].some((p) => p.startsWith(prefix)))
        return sftp.status(id, S.FAILURE);
      files.delete(path);
      sftp.status(id, S.OK);
    });
    sftp.on("RENAME", (id, oldPath, newPath) => {
      if (!files.has(oldPath) || exists(newPath))
        return sftp.status(id, S.FAILURE);
      files.set(newPath, files.get(oldPath));
      files.delete(oldPath);
      sftp.status(id, S.OK);
    });
    sftp.on("REMOVE", (id, path) => {
      if (!files.has(path) || files.get(path).dir)
        return sftp.status(id, S.FAILURE);
      files.delete(path);
      sftp.status(id, S.OK);
    });
  });
}
