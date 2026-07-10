/* Base object for the fixture lib. */

private int _uses;

static void
create()
{
    _uses = 0;
}

public int
visible_fn()
{
    return 1;
}

static int
hidden_fn()
{
    return 2;
}

private int
secret_fn()
{
    return 3;
}
