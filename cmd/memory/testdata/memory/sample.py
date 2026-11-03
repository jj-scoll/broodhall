import os


class Counter:
    """Counts things."""

    def __init__(self):
        self.n = 0

    def bump(self, by=1):
        self.n += by
        return self.n


def scale(x, factor):
    """Scale x by factor."""
    return x * factor
