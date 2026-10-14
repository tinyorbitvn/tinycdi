import fs from "node:fs";
import {
  HARNESS_CONTROL,
  MOCK_API,
  expect,
  login,
  openSession,
  resetState,
  seedReadyWorkspace,
  test,
  watchConsole,
} from "./harness.ts";

// session-transfer: upload then download through the real gateway and
// frontend, against the fake broker and a stub tcdi-xferd. Runs once per
// site layout, so the credentialed CORS request is proven with the
// SameSite=Lax cookie and with the partitioned one (plan ruling 12).
const WS = "ws_e2e0003aabbcc";

test("upload then download a file through the session host", async ({ page, request, harnessMode }) => {
  const violations = watchConsole(page);
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  const ctl = await request.post(`${MOCK_API}/_control/workspaces/fileTransfer`, {
    data: { workspaceId: WS, policy: "Bidirectional", maxFileBytes: 1048576 },
  });
  expect(ctl.ok()).toBeTruthy();
  await login(page);
  await openSession(page, harnessMode, WS);

  await page.getByLabel("Choose a file to upload").setInputFiles({
    name: "Ảnh e2e.txt",
    mimeType: "text/plain",
    buffer: Buffer.from("hello transfer"),
  });
  await expect(page.getByText("Uploaded Ảnh e2e.txt to Transfers.")).toBeVisible();

  await page.getByLabel("File name in the Transfers folder").fill("Ảnh e2e.txt");
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download" }).click()]);
  expect(download.suggestedFilename()).toBe("Ảnh e2e.txt");
  expect(fs.readFileSync(await download.path(), "utf8")).toBe("hello transfer");

  // The gateway records completion after the response is written, so poll.
  const completed = async () => ((await (await request.get(`${HARNESS_CONTROL}/broker/transfers`)).json()) as { completed: unknown[] }).completed;
  await expect.poll(async () => (await completed()).length).toBe(2);
  expect(await completed()).toEqual([
    expect.objectContaining({ bytes: 14, ok: true }),
    expect.objectContaining({ bytes: 14, ok: true }),
  ]);
  expect(violations, "no CSP violations (connect-src reaches the session host)").toEqual([]);
});

test("the controls are hidden when the resolved policy is Disabled", async ({ page, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  await login(page);
  await openSession(page, harnessMode, WS);
  await expect(page.getByRole("region", { name: "File transfer" })).toHaveCount(0);
});

test("an over-limit file is refused before anything is minted", async ({ page, request, harnessMode }) => {
  const mints: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && r.url().endsWith("/file-transfers")) mints.push(r.url());
  });
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  await request.post(`${MOCK_API}/_control/workspaces/fileTransfer`, {
    data: { workspaceId: WS, policy: "Upload", maxFileBytes: 4 },
  });
  await login(page);
  await openSession(page, harnessMode, WS);
  await expect(page.getByRole("button", { name: "Download" })).toHaveCount(0);
  await page.getByLabel("Choose a file to upload").setInputFiles({ name: "big.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(5) });
  await expect(page.getByText("The file is larger than the transfer limit.")).toBeVisible();
  expect(mints, "no grant minted for an over-limit file").toEqual([]);
  const done = await (await request.get(`${HARNESS_CONTROL}/broker/transfers`)).json();
  expect(done.completed).toEqual([]);
});

test("a direction the resolved policy refuses is not offered", async ({ page, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  await request.post(`${MOCK_API}/_control/workspaces/fileTransfer`, {
    data: { workspaceId: WS, policy: "Download", maxFileBytes: 1048576 },
  });
  await login(page);
  await openSession(page, harnessMode, WS);
  await expect(page.getByRole("button", { name: "Download" })).toBeVisible();
  await expect(page.getByLabel("Choose a file to upload")).toHaveCount(0);
});
