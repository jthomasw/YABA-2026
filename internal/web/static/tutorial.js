
"use strict";

document.addEventListener("DOMContentLoaded", () => {
    const tutorialButton = document.getElementById("tutorial-launch");
    const STORAGE_KEY = "yaba_tutorial_step";

    if (!tutorialButton) return;

    // Whether this user can add income and expenses in the budget that's
    // open. The header says so (data-can-add-entries); a viewer can't, and the
    // dashboard doesn't give them the Add buttons the first two steps point
    // at. Missing attribute (a page cached before this change) means "yes",
    // and the dashboard is double-checked below once we're on it.
    let canAddEntries = tutorialButton.dataset.canAddEntries !== "false";
    if (window.location.pathname === "/dashboard" &&
        !document.getElementById("tutorial-add-income") &&
        !document.getElementById("tutorial-add-expense")) {
        canAddEntries = false;
    }

    // ==========================================
    // 1. Tutorial progress
    // ==========================================

    function getTutorialStep() {
        return sessionStorage.getItem(STORAGE_KEY);
    }

    function setTutorialStep(step) {
        sessionStorage.setItem(STORAGE_KEY, step);
    }

    function clearTutorialStep() {
        sessionStorage.removeItem(STORAGE_KEY);
    }

    // ==========================================
    // 2. Clean up tutorial elements
    // ==========================================

    function cleanupTutorial() {
        document.querySelectorAll(".tutorial-highlight").forEach((element) => {
            element.classList.remove("tutorial-highlight");
        });

        document.querySelectorAll(".tutorial-instruction").forEach((element) => {
            element.remove();
        });
    }

    function exitTutorial() {
        cleanupTutorial();
        clearTutorialStep();

        // Remove tutorial URL parameters.
        const url = new URL(window.location.href);
        url.searchParams.delete("tutorial");

        window.history.replaceState(
            {},
            "",
            url.pathname + url.search + url.hash
        );
    }

    // Ends the tutorial with a short note instead of reloading the page to look
    // for something that isn't there. Reloading is what used to trap a viewer
    // in an endless loop: no Add Income button, back to the dashboard, the
    // dashboard resumes the tutorial, still no button, and round again.
    function stopTutorial(message) {
        cleanupTutorial();
        clearTutorialStep();
        showInstruction("Tutorial stopped", message);
    }

    // ==========================================
    // 3. Welcome screen
    // ==========================================

    const dialog = document.createElement("dialog");

    dialog.id = "tutorial-welcome";
    dialog.setAttribute("aria-labelledby", "tutorial-welcome-title");

    dialog.innerHTML = `
        <div class="tutorial-welcome-content">
            <h2 id="tutorial-welcome-title">Welcome to YABA!</h2>

            <p id="tutorial-welcome-intro"></p>

            <p>
                You can skip the tutorial anytime and restart it
                using the ? button next to the theme toggle.
            </p>

            <div class="tutorial-welcome-actions">
                <button
                    type="button"
                    class="btn btn-quiet"
                    id="tutorial-skip"
                >
                    Skip Tutorial
                </button>

                <button
                    type="button"
                    class="btn btn-primary"
                    id="tutorial-start"
                >
                    Start Tutorial
                </button>
            </div>
        </div>
    `;

    // Text, not markup, so nothing here is ever read as HTML.
    dialog.querySelector("#tutorial-welcome-intro").textContent = canAddEntries
        ? "Let's learn how to manage your money using YABA. " +
          "This interactive tutorial will show you how to " +
          "add income, record expenses, and understand your dashboard."
        : "You can view this budget, but only its owner and editors can " +
          "add income or expenses. This short tour shows you around " +
          "the dashboard instead.";

    document.body.appendChild(dialog);

    // Open the tutorial welcome screen.
    tutorialButton.addEventListener("click", () => {
        exitTutorial();
        dialog.showModal();
    });

    // Skip the tutorial.
    document.getElementById("tutorial-skip").addEventListener("click", () => {
        dialog.close();
        exitTutorial();
    });

    // ==========================================
    // 4. Instruction popup
    // ==========================================

    function showInstruction(title, message) {
        document.querySelectorAll(".tutorial-instruction").forEach((element) => {
            element.remove();
        });

        const instruction = document.createElement("div");
        instruction.className = "tutorial-instruction";
        instruction.setAttribute("role", "status");

        const heading = document.createElement("h3");
        heading.textContent = title;

        const paragraph = document.createElement("p");
        paragraph.textContent = message;

        const exitButton = document.createElement("button");
        exitButton.type = "button";
        exitButton.className = "btn btn-quiet";
        exitButton.textContent = "Exit Tutorial";

        exitButton.addEventListener("click", exitTutorial);

        instruction.append(heading, paragraph, exitButton);
        document.body.appendChild(instruction);

        return instruction;
    }

    // ==========================================
    // 5. Highlight Add Income
    // ==========================================

    function highlightIncomeButton() {
        const incomeButton = document.getElementById("tutorial-add-income");

        if (!incomeButton) {
            // Not on the dashboard: go there once, and pick up from there.
            if (window.location.pathname !== "/dashboard") {
                setTutorialStep("income");
                window.location.href = "/dashboard";
                return;
            }
            // On the dashboard and still no button: never reload.
            if (!canAddEntries) {
                showDashboardOverview();
            } else {
                stopTutorial("This step isn't available here. You can start the tutorial again with the ? button.");
            }
            return;
        }

        cleanupTutorial();
        setTutorialStep("income");

        incomeButton.scrollIntoView({
            behavior: "smooth",
            block: "center"
        });

        incomeButton.classList.add("tutorial-highlight");

        showInstruction(
            "Step 1: Add Your First Income",
            "This is where you record money you receive, such as a paycheck, gift, or other income. Click the highlighted green Add Income button to continue!"
        );

        incomeButton.addEventListener("click", () => {
            setTutorialStep("income-form");

            console.log("YABA Tutorial: Moving to Add Income form.");
        }, { once: true });
    }

    // ==========================================
    // 6. Highlight Add Expense
    // ==========================================

    function highlightExpenseButton() {
        const expenseButton = document.getElementById("tutorial-add-expense");

        if (!expenseButton) {
            if (window.location.pathname !== "/dashboard") {
                setTutorialStep("expense");
                window.location.href = "/dashboard?tutorial=expense";
                return;
            }
            if (!canAddEntries) {
                showDashboardOverview();
            } else {
                stopTutorial("This step isn't available here. You can start the tutorial again with the ? button.");
            }
            return;
        }

        cleanupTutorial();
        setTutorialStep("expense");

        expenseButton.scrollIntoView({
            behavior: "smooth",
            block: "center"
        });

        expenseButton.classList.add("tutorial-highlight");

        showInstruction(
            "Step 2: Add an Expense",
            "Now let's record money you've spent. Click the highlighted Add Expense button to continue. You can choose a one-time or recurring expense."
        );

        expenseButton.addEventListener("click", () => {
            setTutorialStep("expense-form");

            console.log("YABA Tutorial: Moving to Add Expense form.");
        }, { once: true });
    }

    // ==========================================
    // 7. Dashboard overview
    // ==========================================

    function showDashboardOverview() {
        if (window.location.pathname !== "/dashboard") {
            setTutorialStep("overview");
            window.location.href = "/dashboard?tutorial=overview";
            return;
        }

        cleanupTutorial();
        setTutorialStep("overview");

        const instruction = canAddEntries
            ? showInstruction(
                "Step 3: Your Dashboard",
                "Great job! This is your Dashboard, where you can review your income, expenses, and overall budget. You've completed the YABA tutorial!"
            )
            : showInstruction(
                "Your Dashboard",
                "This is the Dashboard for the budget you're viewing. The tabs show its current funds, income, expenses and emergency fund, and View Transactions lists every entry. Only the owner and editors can add income or expenses, so those buttons aren't shown to you."
            );

        const finishButton = document.createElement("button");

        finishButton.type = "button";
        finishButton.className = "btn btn-primary";
        finishButton.textContent = "Finish Tutorial";

        finishButton.addEventListener("click", () => {
            exitTutorial();
        });

        instruction.appendChild(finishButton);
    }

    // ==========================================
    // 8. Start tutorial
    // ==========================================

    document.getElementById("tutorial-start").addEventListener("click", () => {
        dialog.close();

        if (canAddEntries) {
            setTutorialStep("income");
            highlightIncomeButton();
        } else {
            // A viewer skips the two "add" steps they can't do.
            showDashboardOverview();
        }
    });

    // ==========================================
    // 9. Resume tutorial after navigation
    // ==========================================

    const tutorialParams = new URLSearchParams(window.location.search);
    const requestedStep = tutorialParams.get("tutorial");
    const currentStep = getTutorialStep();
    const currentPath = window.location.pathname;

    // The backend redirects here after saving income.
    if (requestedStep === "expense" && currentPath === "/dashboard") {
        highlightExpenseButton();
        return;
    }

    // The backend redirects here after saving an expense.
    if (requestedStep === "overview" && currentPath === "/dashboard") {
        showDashboardOverview();
        return;
    }

    // Resume Step 1 on the Dashboard.
    if (currentStep === "income" && currentPath === "/dashboard") {
        highlightIncomeButton();
        return;
    }

    // Resume Step 2 on the Dashboard.
    if (currentStep === "expense" && currentPath === "/dashboard") {
        highlightExpenseButton();
        return;
    }

    // Resume the final overview.
    if (currentStep === "overview" && currentPath === "/dashboard") {
        showDashboardOverview();
        return;
    }

    // ==========================================
    // 10. Income form
    // ==========================================

    if (currentStep === "income-form" && currentPath === "/income") {
        console.log("YABA Tutorial: Income form reached.");

        const tutorialField = document.getElementById("tutorial-step");

        if (tutorialField) {
            tutorialField.value = "income-form";
        }
    }

    // ==========================================
    // 11. Expense form
    // ==========================================

    if (currentStep === "expense-form" && currentPath === "/expense") {
        console.log("YABA Tutorial: Expense form reached.");

        const tutorialField = document.getElementById("tutorial-step");

        if (tutorialField) {
            tutorialField.value = "expense-form";
        }
    }
});
