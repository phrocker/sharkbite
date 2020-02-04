package org.poma.accumulo;

import org.apache.commons.pool2.BasePooledObjectFactory;
import org.apache.commons.pool2.PooledObject;
import org.apache.commons.pool2.impl.DefaultPooledObject;
import org.python.util.PythonInterpreter;

public class PythonInterpreterFactory extends BasePooledObjectFactory<PythonInterpreter> {
    @Override
    public PythonInterpreter create() throws Exception {
        return new PythonInterpreter();
    }

    @Override
    public PooledObject<PythonInterpreter> wrap(PythonInterpreter pythonInterpreter) {
        return new DefaultPooledObject<>(pythonInterpreter);
    }

    @Override
    public void destroyObject(PooledObject<PythonInterpreter> p)
            throws Exception{
        p.getObject().close();
    }
    @Override
    public void passivateObject(PooledObject<PythonInterpreter> p) throws Exception {
        p.getObject().cleanup();
    }
}
