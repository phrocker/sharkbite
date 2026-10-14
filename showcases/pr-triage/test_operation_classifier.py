# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import unittest
from operation_classifier import fit,score,restore
import context_classifier as cc

class OperationClassifierTest(unittest.TestCase):
    def test_svm_roundtrip_and_incomplete_evidence_retains(self):
        rows=[{'unit_id':str(i),'label':label,'operation_complete':True,'text':{'operations':text}} for i,(label,text) in enumerate([
            ('review','CONSTRUCT Key KEY ColumnVisibility FROM scope'),('review','ASSIGN request Identity FROM principal'),
            ('routine','RETURN fmt Sprintf text value'),('routine','CALL sort Strings names')])]
        model=fit(rows,('operations','linear_svm'));params=cc.bundle(model,('code','mixed',3.));params.pop('recipe')
        self.assertEqual(score(model,rows,('operations','linear_svm')),score(restore({'parameters':params}),rows,('operations','linear_svm')))
        rows[0]['operation_complete']=False
        self.assertIsNone(score(model,rows,('operations','linear_svm'))['0'])

if __name__=='__main__':unittest.main()
