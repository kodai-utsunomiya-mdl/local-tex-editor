import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

// Go links this COFF resource object into the Windows executable. The ICO is
// checked in, so the build needs only Node and Go, already used by the project.
// Format: https://learn.microsoft.com/windows/win32/debug/pe-format
const iconPath = new URL('../internal/branding/app-icon.ico', import.meta.url);
const outputPath = new URL('../cmd/local-tex-editor/icon_windows_amd64.syso', import.meta.url);
const ico = await readFile(iconPath);
const count = ico.length >= 6 ? ico.readUInt16LE(4) : 0;
if (ico.length < 6 || ico.readUInt16LE(0) !== 0 || ico.readUInt16LE(2) !== 1
  || count < 1 || count > 256 || ico.length < 6 + 16 * count) {
  throw new Error('app-icon.ico has an invalid icon directory');
}

const icons = Array.from({ length: count }, (_, index) => {
  const entry = ico.subarray(6 + index * 16, 22 + index * 16);
  const size = entry.readUInt32LE(8);
  const offset = entry.readUInt32LE(12);
  if (!size || offset < 6 + count * 16 || offset + size > ico.length) {
    throw new Error(`Icon ${index + 1} refers to data outside app-icon.ico`);
  }
  return { id: index + 1, entry, data: ico.subarray(offset, offset + size) };
});

const group = Buffer.alloc(6 + 14 * count);
ico.copy(group, 0, 0, 6);
for (const [index, icon] of icons.entries()) {
  icon.entry.copy(group, 6 + index * 14, 0, 12);
  group.writeUInt16LE(icon.id, 18 + index * 14);
}

// Resource directories use type, numeric ID, then language. Directory offsets
// are relative to .rsrc. Only data RVAs need relocation by the Go linker.
const types = [
  { id: 3, resources: icons }, // RT_ICON
  { id: 14, resources: [{ id: 1, data: group }] }, // RT_GROUP_ICON
];
let cursor = 16 + types.length * 8;
for (const type of types) {
  type.offset = cursor;
  cursor += 16 + type.resources.length * 8;
}
for (const type of types) {
  for (const resource of type.resources) {
    resource.languageOffset = cursor;
    cursor += 24;
  }
}
const resources = types.flatMap(type => type.resources);
for (const resource of resources) {
  resource.entryOffset = cursor;
  cursor += 16;
}
const align4 = value => (value + 3) & ~3;
cursor = align4(cursor);
for (const resource of resources) {
  resource.dataOffset = cursor;
  cursor = align4(cursor + resource.data.length);
}
const section = Buffer.alloc(cursor);
function directory(offset, entries) {
  section.writeUInt16LE(entries.length, offset + 14);
  entries.forEach((entry, index) => {
    section.writeUInt32LE(entry.id, offset + 16 + index * 8);
    section.writeUInt32LE(entry.offset + (entry.directory ? 0x80000000 : 0), offset + 20 + index * 8);
  });
}
directory(0, types.map(type => ({ id: type.id, offset: type.offset, directory: true })));
for (const type of types) {
  directory(type.offset, type.resources.map(resource => ({ id: resource.id, offset: resource.languageOffset, directory: true })));
  for (const resource of type.resources) {
    directory(resource.languageOffset, [{ id: 0, offset: resource.entryOffset }]); // Language-neutral artwork.
    section.writeUInt32LE(resource.dataOffset, resource.entryOffset);
    section.writeUInt32LE(resource.data.length, resource.entryOffset + 4);
    resource.data.copy(section, resource.dataOffset);
  }
}

const headerSize = 20 + 40;
const relocationOffset = headerSize + section.length;
const symbolOffset = relocationOffset + resources.length * 10;
const object = Buffer.alloc(symbolOffset + 18 + 4);
object.writeUInt16LE(0x8664, 0); // IMAGE_FILE_MACHINE_AMD64
object.writeUInt16LE(1, 2);
object.writeUInt32LE(symbolOffset, 8);
object.writeUInt32LE(1, 12);
object.write('.rsrc', 20, 'ascii');
object.writeUInt32LE(section.length, 36);
object.writeUInt32LE(headerSize, 40);
object.writeUInt32LE(relocationOffset, 44);
object.writeUInt16LE(resources.length, 52);
object.writeUInt32LE(0x40300040, 56); // Initialized, readable data, aligned to four bytes.
section.copy(object, headerSize);
resources.forEach((resource, index) => {
  const offset = relocationOffset + index * 10;
  object.writeUInt32LE(resource.entryOffset, offset);
  object.writeUInt16LE(0x0003, offset + 8); // IMAGE_REL_AMD64_ADDR32NB, symbol 0.
});
object.write('.rsrc', symbolOffset, 'ascii');
object.writeUInt16LE(1, symbolOffset + 12); // Section 1.
object.writeUInt8(3, symbolOffset + 16); // IMAGE_SYM_CLASS_STATIC.
object.writeUInt32LE(4, symbolOffset + 18); // Empty COFF string table.
await writeFile(outputPath, object);
console.log(`Embedded ${count} icon sizes in ${fileURLToPath(outputPath)}`);
