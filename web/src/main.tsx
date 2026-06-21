import React from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider } from "react-router-dom";
import { AppShell } from "./app/AppShell";
import { Sessions } from "./screens/Sessions";
import { NewSession } from "./screens/NewSession";
import { Workspace } from "./screens/Workspace";
import { Restore } from "./screens/Restore";
import "./design/tokens.css";

// NewSession is a modal rendered over the Sessions console, so /new reuses the
// Sessions element underneath via a layout child that renders both.
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
      { path: "/restore/:id", element: <Restore /> },
    ],
  },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>
);
