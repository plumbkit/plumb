"""Export recorded charges as CSV rows (structure-only language for the graph)."""


def format_row(index, cents):
    return f"{index},{cents / 100:.2f}"


def export_charges(entries):
    return "\n".join(format_row(i, c) for i, c in enumerate(entries))
