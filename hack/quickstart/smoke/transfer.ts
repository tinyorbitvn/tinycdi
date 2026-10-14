import * as fs from "node:fs";
import { expect, type Page } from "@playwright/test";

// Upload then download one file through the session page's transfer bar,
// checking the bytes on the pod's disk on both legs, then try a name that
// would escape the Transfers folder. `inHome(script, ...args)` runs
// `sh -c script _ args…` in the workspace's desktop container (kubectl
// exec); `transfersDir` is the folder inside the pod ($HOME/transfers).
export async function transferRoundTrip(
  page: Page,
  stamp: string,
  transfersDir: string,
  inHome: (script: string, ...args: string[]) => string,
) {
  const up = `up-${stamp}.txt`;
  await page.getByLabel("Choose a file to upload").setInputFiles({
    name: up,
    mimeType: "text/plain",
    buffer: Buffer.from(`hello ${stamp}`),
  });
  await expect(page.getByText(`Uploaded ${up} to Transfers.`)).toBeVisible({ timeout: 60_000 });
  expect(inHome(`cat "${transfersDir}/$1"`, up)).toBe(`hello ${stamp}`);

  const down = `down-${stamp}.txt`;
  inHome(`printf 'from the pod %s' "$2" > "${transfersDir}/$1"`, down, stamp);
  await page.getByLabel("File name in the Transfers folder").fill(down);
  const [dl] = await Promise.all([
    page.waitForEvent("download", { timeout: 60_000 }),
    page.getByRole("button", { name: "Download" }).click(),
  ]);
  expect(dl.suggestedFilename()).toBe(down);
  expect(fs.readFileSync(await dl.path(), "utf8")).toBe(`from the pod ${stamp}`);

  // An escape attempt never reaches the API: the name is refused client-side
  // (the server and the in-pod jail refuse it too, Tasks 11-13).
  await page.getByLabel("File name in the Transfers folder").fill("../.bashrc");
  await page.getByRole("button", { name: "Download" }).click();
  await expect(page.getByText("Use a single file name without folders.")).toBeVisible();
  console.log(`file transfer round-trip ok: ${up} up, ${down} down`);
}
