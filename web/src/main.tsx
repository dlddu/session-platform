import React from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider } from "react-router-dom";
import { AppShell } from "./app/AppShell";
import { Sessions } from "./screens/Sessions";
import { NewSession } from "./screens/NewSession";
import { Workspace } from "./screens/Workspace";
import { Restore } from "./screens/Restore";
import "./design/tokens.css";

// /new 는 Sessions 위에 얹히는 모달이라 두 엘리먼트를 함께 렌더한다.
const router = createBrowserRouter([
  {
    element: <AppShell />,
    children: [
      { path: "/", element: <Sessions /> },
      {
        path: "/new",
        element: (
          <>
            <Sessions />
            <NewSession />
          </>
        ),
      },
      { path: "/session/:id", element: <Workspace /> },
      { path: "/agent/:id", element: <Workspace /> },
      { path: "/gated/:id", element: <Workspace /> },
      { path: "/restore/:id", element: <Restore /> },
    ],
  },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>
);
